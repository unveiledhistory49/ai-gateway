package test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/company/ai-gateway/internal/auth"
	"github.com/company/ai-gateway/internal/config"
	"github.com/company/ai-gateway/internal/dispatcher"
	"github.com/company/ai-gateway/internal/model"
	"github.com/company/ai-gateway/internal/pipeline"
	"github.com/company/ai-gateway/internal/resilience"
	"github.com/company/ai-gateway/internal/router"
	"github.com/company/ai-gateway/internal/server"
)

type resilienceE2ESuite struct {
	primaryServer   *httptest.Server
	secondaryServer *httptest.Server
	gatewayServer   *server.Server
	registry        *resilience.Registry
	primaryCalls    int32
	secondaryCalls  int32
	timeLock        sync.Mutex
	fakeNow         time.Time
}

func setupResilienceE2E(t *testing.T, cbConfig resilience.CircuitBreakerConfig) *resilienceE2ESuite {
	suite := &resilienceE2ESuite{
		fakeNow: time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC),
	}

	cbConfig.NowFunc = func() time.Time {
		suite.timeLock.Lock()
		defer suite.timeLock.Unlock()
		return suite.fakeNow
	}
	suite.registry = resilience.NewRegistry(cbConfig, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// 1. Primary Upstream Provider (Tier 0)
	suite.primaryServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&suite.primaryCalls, 1)

		var req model.CanonicalChatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)

		msg := ""
		if len(req.Messages) > 0 {
			msg = req.Messages[0].ContentString()
		}

		if msg == "force-503" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"error":{"message":"Primary overloaded 503"}}`))
			return
		}

		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher := w.(http.Flusher)

			if msg == "stream-fail-after-flush" {
				w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"start\"}}]}\n\n"))
				flusher.Flush()
				if hj, ok := w.(http.Hijacker); ok {
					conn, _, _ := hj.Hijack()
					conn.Close()
				}
				return
			}

			w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"primary-stream\"}}]}\n\n"))
			flusher.Flush()
			w.Write([]byte("data: [DONE]\n\n"))
			flusher.Flush()
			return
		}

		// Normal sync 200
		resp := model.CanonicalChatResponse{
			ID:    "chatcmpl-primary",
			Model: req.Model,
			Choices: []model.ChatChoice{
				{
					Index: 0,
					Message: model.ChatMessage{
						Role:    "assistant",
						Content: "response from primary",
					},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(resp)
	}))

	// 2. Secondary Upstream Provider (Tier 1 Fallback)
	suite.secondaryServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&suite.secondaryCalls, 1)

		var req model.CanonicalChatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)

		msg := ""
		if len(req.Messages) > 0 {
			msg = req.Messages[0].ContentString()
		}

		if msg == "secondary-503" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"error":{"message":"Secondary overloaded 503"}}`))
			return
		}

		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher := w.(http.Flusher)
			w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"secondary-stream\"}}]}\n\n"))
			flusher.Flush()
			w.Write([]byte("data: [DONE]\n\n"))
			flusher.Flush()
			return
		}

		resp := model.CanonicalChatResponse{
			ID:    "chatcmpl-secondary",
			Model: req.Model,
			Choices: []model.ChatChoice{
				{
					Index: 0,
					Message: model.ChatMessage{
						Role:    "assistant",
						Content: "response from secondary",
					},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(resp)
	}))

	// Configuration with Tier 0 Primary and Tier 1 Secondary Fallback
	yamlConfig := fmt.Sprintf(`
server:
  host: "127.0.0.1"
  port: 8080
upstreams:
  - id: "openai-primary"
    provider: "openai"
    endpoint_url: "%s"
    timeout_seconds: 5
  - id: "anthropic-backup"
    provider: "anthropic"
    endpoint_url: "%s"
    timeout_seconds: 5
routes:
  - alias: "prod-chat"
    tiers:
      - priority: 0
        targets:
          - upstream_id: "openai-primary"
            model: "gpt-4o"
      - priority: 1
        targets:
          - upstream_id: "anthropic-backup"
            model: "claude-3-5"
tenants:
  - id: "tenant-prod"
    api_keys:
      - "sk-resilience-key"
    allowed_routes:
      - "prod-chat"
`, suite.primaryServer.URL, suite.secondaryServer.URL)

	cfg, err := config.ParseConfig(yamlConfig)
	if err != nil {
		t.Fatalf("failed to parse config: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	authStage := auth.NewStage(cfg.Tenants, cfg.Server.MaxBodyBytes)
	routeStage := router.NewStage(cfg, suite.registry)
	dispStage := dispatcher.NewStageWithClient(suite.primaryServer.Client(), suite.registry)

	pipe := pipeline.NewPipeline(authStage, routeStage, dispStage)
	suite.gatewayServer = server.NewServerWithPipeline(cfg, pipe, authStage, logger)
	suite.gatewayServer.SetRegistry(suite.registry)

	return suite
}

func (s *resilienceE2ESuite) close() {
	s.primaryServer.Close()
	s.secondaryServer.Close()
}

func (s *resilienceE2ESuite) advanceTime(d time.Duration) {
	s.timeLock.Lock()
	defer s.timeLock.Unlock()
	s.fakeNow = s.fakeNow.Add(d)
}

// 1. Primary upstream failing with 503 -> traffic automatically fails over to secondary fallback upstream
func TestE2EFailoverToFallbackOn503(t *testing.T) {
	ts := setupResilienceE2E(t, resilience.CircuitBreakerConfig{
		MinRequests: 20,
	})
	defer ts.close()

	// Send request where primary returns 503
	reqBody := `{"model":"prod-chat","messages":[{"role":"user","content":"force-503"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(reqBody))
	req.Header.Set("Authorization", "Bearer sk-resilience-key")
	rec := httptest.NewRecorder()

	ts.gatewayServer.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK after automatic failover, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify response was served by secondary
	var chatResp model.CanonicalChatResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &chatResp)
	if len(chatResp.Choices) == 0 || chatResp.Choices[0].Message.Content != "response from secondary" {
		t.Fatalf("expected content from secondary fallback, got: %v", chatResp.Choices)
	}

	if atomic.LoadInt32(&ts.primaryCalls) != 1 {
		t.Fatalf("expected primary to be called 1 time, got %d", ts.primaryCalls)
	}
	if atomic.LoadInt32(&ts.secondaryCalls) != 1 {
		t.Fatalf("expected secondary to be called 1 time, got %d", ts.secondaryCalls)
	}
}

// 2. Primary circuit breaker tripping -> fast-failing to fallback target without latency penalty
func TestE2ECircuitBreakerTrippingFastFail(t *testing.T) {
	ts := setupResilienceE2E(t, resilience.CircuitBreakerConfig{
		MinRequests: 5,
		Cooldown:    30 * time.Second,
	})
	defer ts.close()

	// Send 5 failures to trip primary circuit breaker
	for i := 0; i < 5; i++ {
		reqBody := `{"model":"prod-chat","messages":[{"role":"user","content":"force-503"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(reqBody))
		req.Header.Set("Authorization", "Bearer sk-resilience-key")
		rec := httptest.NewRecorder()
		ts.gatewayServer.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200 via fallback, got %d", i, rec.Code)
		}
	}

	cbPrimary := ts.registry.Get("openai-primary")
	if cbPrimary.State() != resilience.StateOpen {
		t.Fatalf("expected primary breaker to be OPEN after 5 failures, got %s", cbPrimary.State())
	}

	primaryCallsBefore := atomic.LoadInt32(&ts.primaryCalls)
	secondaryCallsBefore := atomic.LoadInt32(&ts.secondaryCalls)

	// Now send request with normal prompt
	reqBody := `{"model":"prod-chat","messages":[{"role":"user","content":"normal-prompt"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(reqBody))
	req.Header.Set("Authorization", "Bearer sk-resilience-key")
	rec := httptest.NewRecorder()
	ts.gatewayServer.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK via fast-fallback, got %d", rec.Code)
	}

	// Because primary breaker is OPEN, router fast-fails past primary directly to secondary!
	// Primary should NOT have received another request!
	if atomic.LoadInt32(&ts.primaryCalls) != primaryCallsBefore {
		t.Fatalf("fast-fail violation: primary was called despite OPEN breaker!")
	}
	if atomic.LoadInt32(&ts.secondaryCalls) != secondaryCallsBefore+1 {
		t.Fatalf("expected secondary to handle the fast-failed request")
	}
}

// 3. Recovery of primary upstream via canary probes
func TestE2ERecoveryViaCanaryProbes(t *testing.T) {
	ts := setupResilienceE2E(t, resilience.CircuitBreakerConfig{
		MinRequests:      2,
		Cooldown:         30 * time.Second,
		SuccessThreshold: 5,
		MaxCanaryProbes:  1,
	})
	defer ts.close()

	// 1. Trip primary breaker
	cbPrimary := ts.registry.Get("openai-primary")
	cbPrimary.RecordFailure()
	cbPrimary.RecordFailure()

	if cbPrimary.State() != resilience.StateOpen {
		t.Fatalf("expected primary to be OPEN, got %s", cbPrimary.State())
	}

	// 2. Advance time past 30s cooldown
	ts.advanceTime(31 * time.Second)

	// 3. Next 5 requests should route to primary as canary probes and succeed
	for i := 0; i < 5; i++ {
		reqBody := `{"model":"prod-chat","messages":[{"role":"user","content":"normal-prompt"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(reqBody))
		req.Header.Set("Authorization", "Bearer sk-resilience-key")
		rec := httptest.NewRecorder()
		ts.gatewayServer.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("canary probe %d: expected 200 OK, got %d", i+1, rec.Code)
		}
	}

	// After 5 successful canary probes, primary breaker must recover to CLOSED!
	if cbPrimary.State() != resilience.StateClosed {
		t.Fatalf("expected primary to recover to CLOSED after 5 successful canaries, got %s", cbPrimary.State())
	}
}

// 4. All upstreams failing -> returns 503 CIRCUIT_OPEN
func TestE2EAllUpstreamsFailingReturns503(t *testing.T) {
	ts := setupResilienceE2E(t, resilience.CircuitBreakerConfig{
		MinRequests: 1,
	})
	defer ts.close()

	// Trip both primary and secondary breakers
	cbPrimary := ts.registry.Get("openai-primary")
	cbPrimary.RecordFailure()

	cbSecondary := ts.registry.Get("anthropic-backup")
	cbSecondary.RecordFailure()

	if cbPrimary.State() != resilience.StateOpen || cbSecondary.State() != resilience.StateOpen {
		t.Fatalf("expected both breakers to be OPEN")
	}

	reqBody := `{"model":"prod-chat","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(reqBody))
	req.Header.Set("Authorization", "Bearer sk-resilience-key")
	rec := httptest.NewRecorder()

	ts.gatewayServer.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 Service Unavailable when all targets OPEN, got %d: %s", rec.Code, rec.Body.String())
	}

	var errResp model.ErrorResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &errResp)
	if errResp.Error.Code != model.ErrCodeCircuitOpen {
		t.Fatalf("expected %s error code, got %s", model.ErrCodeCircuitOpen, errResp.Error.Code)
	}
}

// 5. Streaming retry safety check: if streaming chunks have already been flushed, do NOT retry
func TestE2EStreamingSafetyNoRetryAfterFlush(t *testing.T) {
	ts := setupResilienceE2E(t, resilience.CircuitBreakerConfig{
		MinRequests: 20,
	})
	defer ts.close()

	// Send streaming request where primary drops after flushing initial chunk
	reqBody := `{"model":"prod-chat","messages":[{"role":"user","content":"stream-fail-after-flush"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(reqBody))
	req.Header.Set("Authorization", "Bearer sk-resilience-key")
	rec := httptest.NewRecorder()

	ts.gatewayServer.Handler().ServeHTTP(rec, req)

	// Since chunks were already flushed, secondary MUST NEVER be invoked!
	if atomic.LoadInt32(&ts.secondaryCalls) != 0 {
		t.Fatalf("CRITICAL SAFETY VIOLATION: secondary fallback was called (%d times) after streaming chunks were already flushed downstream!", ts.secondaryCalls)
	}

	// Verify downstream received initial chunk and error frame
	bodyStr := rec.Body.String()
	if !strings.Contains(bodyStr, "start") {
		t.Fatalf("expected initial chunk in output, got: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, "upstream_failure") {
		t.Fatalf("expected error frame in output, got: %s", bodyStr)
	}
}
