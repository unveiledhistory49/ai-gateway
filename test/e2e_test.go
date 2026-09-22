package test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/company/ai-gateway/internal/auth"
	"github.com/company/ai-gateway/internal/config"
	"github.com/company/ai-gateway/internal/dispatcher"
	"github.com/company/ai-gateway/internal/model"
	"github.com/company/ai-gateway/internal/pipeline"
	"github.com/company/ai-gateway/internal/router"
	"github.com/company/ai-gateway/internal/server"
)

type testSuite struct {
	mockUpstream      *httptest.Server
	gatewayServer     *server.Server
	upstreamCancelled int32
}

func setupE2E(t *testing.T) *testSuite {
	ts := &testSuite{}

	// Mock Upstream Provider
	ts.mockUpstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify vaulted upstream key was injected
		if r.Header.Get("Authorization") != "Bearer sk-vault-upstream-123" {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":{"message":"Invalid upstream token"}}`))
			return
		}

		var chatReq model.CanonicalChatRequest
		if err := json.NewDecoder(r.Body).Decode(&chatReq); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// Verify model mapping occurred
		if chatReq.Model != "gpt-4o-2024-08-06" && chatReq.Model != "claude-3-5" {
			http.Error(w, fmt.Sprintf("unexpected model: %s", chatReq.Model), http.StatusBadRequest)
			return
		}

		userMsg := ""
		if len(chatReq.Messages) > 0 {
			userMsg = chatReq.Messages[0].ContentString()
		}

		// Handle simulated 502/503
		if userMsg == "simulate-502" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			w.Write([]byte(`{"error":{"message":"Mock upstream capacity exhausted 502"}}`))
			return
		}
		if userMsg == "simulate-503" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"error":{"message":"Mock upstream overloaded 503"}}`))
			return
		}

		// Handle Streaming SSE
		if chatReq.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, ok := w.(http.Flusher)
			if !ok {
				http.Error(w, "flushing not supported", http.StatusInternalServerError)
				return
			}

			if userMsg == "slow-stream" {
				w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"start\"}}]}\n\n"))
				flusher.Flush()

				select {
				case <-r.Context().Done():
					atomic.StoreInt32(&ts.upstreamCancelled, 1)
					return
				case <-time.After(5 * time.Second):
					return
				}
			}

			w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n"))
			flusher.Flush()
			time.Sleep(10 * time.Millisecond)

			w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\" World!\"}}]}\n\n"))
			flusher.Flush()
			time.Sleep(10 * time.Millisecond)

			w.Write([]byte("data: [DONE]\n\n"))
			flusher.Flush()
			return
		}

		// Handle Synchronous JSON
		resp := model.CanonicalChatResponse{
			ID:      "chatcmpl-e2e-sync",
			Object:  "chat.completion",
			Created: 1715644800,
			Model:   chatReq.Model,
			Choices: []model.ChatChoice{
				{
					Index: 0,
					Message: model.ChatMessage{
						Role:    "assistant",
						Content: "Sync completion response",
					},
				},
			},
			Usage: &model.UsageInfo{
				PromptTokens:     10,
				CompletionTokens: 5,
				TotalTokens:      15,
			},
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(resp)
	}))

	// Gateway Configuration
	yamlConfig := fmt.Sprintf(`
server:
  host: "127.0.0.1"
  port: 8080
upstreams:
  - id: "mock-upstream"
    provider: "openai"
    endpoint_url: "%s"
    api_key: "sk-vault-upstream-123"
    timeout_seconds: 5
routes:
  - alias: "gpt-4o"
    primary_upstream: "mock-upstream"
    model_name: "gpt-4o-2024-08-06"
  - alias: "claude-3-5"
    primary_upstream: "mock-upstream"
    model_name: "claude-3-5"
tenants:
  - id: "tenant-prod"
    name: "Production Tenant"
    tier: "production"
    api_keys:
      - "sk-gw-tenant-prod-key"
    allowed_routes:
      - "gpt-4o"
`, ts.mockUpstream.URL)

	cfg, err := config.ParseConfig(yamlConfig)
	if err != nil {
		t.Fatalf("failed to parse gateway config: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	authStage := auth.NewStage(cfg.Tenants, cfg.Server.MaxBodyBytes)
	routeStage := router.NewStage(cfg)
	dispStage := dispatcher.NewStageWithClient(ts.mockUpstream.Client())

	pipe := pipeline.NewPipeline(authStage, routeStage, dispStage)
	ts.gatewayServer = server.NewServerWithPipeline(cfg, pipe, authStage, logger)

	return ts
}

func TestE2EMissingOrInvalidAPIKey(t *testing.T) {
	ts := setupE2E(t)
	defer ts.mockUpstream.Close()

	// 1. Missing Authorization header
	req1 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"gpt-4o"}`))
	rec1 := httptest.NewRecorder()
	ts.gatewayServer.Handler().ServeHTTP(rec1, req1)

	if rec1.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for missing key, got %d", rec1.Code)
	}
	var errResp1 model.ErrorResponse
	json.Unmarshal(rec1.Body.Bytes(), &errResp1)
	if errResp1.Error.Code != model.ErrCodeMissingOrInvalidAPIKey {
		t.Fatalf("expected %s, got %s", model.ErrCodeMissingOrInvalidAPIKey, errResp1.Error.Code)
	}

	// 2. Invalid Authorization key
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"gpt-4o"}`))
	req2.Header.Set("Authorization", "Bearer sk-gw-completely-bogus-key")
	rec2 := httptest.NewRecorder()
	ts.gatewayServer.Handler().ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for invalid key, got %d", rec2.Code)
	}
	var errResp2 model.ErrorResponse
	json.Unmarshal(rec2.Body.Bytes(), &errResp2)
	if errResp2.Error.Code != model.ErrCodeMissingOrInvalidAPIKey {
		t.Fatalf("expected %s, got %s", model.ErrCodeMissingOrInvalidAPIKey, errResp2.Error.Code)
	}
}

func TestE2EForbiddenAndUnknownRoutes(t *testing.T) {
	ts := setupE2E(t)
	defer ts.mockUpstream.Close()

	// Forbidden route: tenant-prod does not have access to claude-3-5
	bodyForbidden := `{"model":"claude-3-5","messages":[{"role":"user","content":"hello"}]}`
	req1 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(bodyForbidden))
	req1.Header.Set("Authorization", "Bearer sk-gw-tenant-prod-key")
	rec1 := httptest.NewRecorder()
	ts.gatewayServer.Handler().ServeHTTP(rec1, req1)

	if rec1.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden, got %d: %s", rec1.Code, rec1.Body.String())
	}
	var errResp1 model.ErrorResponse
	json.Unmarshal(rec1.Body.Bytes(), &errResp1)
	if errResp1.Error.Code != model.ErrCodeForbiddenRoute {
		t.Fatalf("expected %s, got %s", model.ErrCodeForbiddenRoute, errResp1.Error.Code)
	}

	// Unknown route: route not mapped in gateway
	bodyUnknown := `{"model":"non-existent-route","messages":[{"role":"user","content":"hello"}]}`
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(bodyUnknown))
	req2.Header.Set("Authorization", "Bearer sk-gw-tenant-prod-key")
	rec2 := httptest.NewRecorder()
	ts.gatewayServer.Handler().ServeHTTP(rec2, req2)

	// In Auth, if tenant doesn't have route allowed, it returns 403.
	if rec2.Code != http.StatusForbidden && rec2.Code != http.StatusNotFound {
		t.Fatalf("expected 403 or 404 for unmapped route, got %d: %s", rec2.Code, rec2.Body.String())
	}
}

func TestE2EValidAPIKeySyncJSON(t *testing.T) {
	ts := setupE2E(t)
	defer ts.mockUpstream.Close()

	reqBody := `{"model":"gpt-4o","messages":[{"role":"user","content":"test synchronous completion"}],"stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(reqBody))
	req.Header.Set("Authorization", "Bearer sk-gw-tenant-prod-key")
	rec := httptest.NewRecorder()

	ts.gatewayServer.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}

	contentType := rec.Header().Get("Content-Type")
	if !strings.Contains(contentType, "application/json") {
		t.Fatalf("expected application/json, got %s", contentType)
	}

	var chatResp model.CanonicalChatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &chatResp); err != nil {
		t.Fatalf("failed to decode json response: %v", err)
	}

	if len(chatResp.Choices) == 0 || chatResp.Choices[0].Message.Content != "Sync completion response" {
		t.Fatalf("unexpected choice content: %v", chatResp.Choices)
	}
	if chatResp.Usage == nil || chatResp.Usage.TotalTokens != 15 {
		t.Fatalf("unexpected usage info: %v", chatResp.Usage)
	}
}

func TestE2EValidAPIKeySSEStreaming(t *testing.T) {
	ts := setupE2E(t)
	defer ts.mockUpstream.Close()

	reqBody := `{"model":"gpt-4o","messages":[{"role":"user","content":"test streaming completion"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(reqBody))
	req.Header.Set("Authorization", "Bearer sk-gw-tenant-prod-key")
	rec := httptest.NewRecorder()

	ts.gatewayServer.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}

	contentType := rec.Header().Get("Content-Type")
	if !strings.Contains(contentType, "text/event-stream") {
		t.Fatalf("expected text/event-stream, got %s", contentType)
	}

	// Verify received chunks
	bodyStr := rec.Body.String()
	if !strings.Contains(bodyStr, "Hello") || !strings.Contains(bodyStr, " World!") || !strings.Contains(bodyStr, "[DONE]") {
		t.Fatalf("expected SSE body to contain streamed chunks, got: %s", bodyStr)
	}
}

func TestE2EUpstreamErrors(t *testing.T) {
	ts := setupE2E(t)
	defer ts.mockUpstream.Close()

	// 1. Simulate 502 Bad Gateway
	reqBody502 := `{"model":"gpt-4o","messages":[{"role":"user","content":"simulate-502"}],"stream":false}`
	req1 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(reqBody502))
	req1.Header.Set("Authorization", "Bearer sk-gw-tenant-prod-key")
	rec1 := httptest.NewRecorder()
	ts.gatewayServer.Handler().ServeHTTP(rec1, req1)

	if rec1.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 Bad Gateway, got %d: %s", rec1.Code, rec1.Body.String())
	}
	var errResp1 model.ErrorResponse
	json.Unmarshal(rec1.Body.Bytes(), &errResp1)
	if errResp1.Error.Code != model.ErrCodeUpstreamBadGateway {
		t.Fatalf("expected %s, got %s", model.ErrCodeUpstreamBadGateway, errResp1.Error.Code)
	}

	// 2. Simulate 503 Service Unavailable
	reqBody503 := `{"model":"gpt-4o","messages":[{"role":"user","content":"simulate-503"}],"stream":false}`
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(reqBody503))
	req2.Header.Set("Authorization", "Bearer sk-gw-tenant-prod-key")
	rec2 := httptest.NewRecorder()
	ts.gatewayServer.Handler().ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 Service Unavailable, got %d: %s", rec2.Code, rec2.Body.String())
	}
	var errResp2 model.ErrorResponse
	json.Unmarshal(rec2.Body.Bytes(), &errResp2)
	if errResp2.Error.Code != model.ErrCodeUpstreamUnavailable {
		t.Fatalf("expected %s, got %s", model.ErrCodeUpstreamUnavailable, errResp2.Error.Code)
	}
}

func TestE2EContextCancellationDuringStream(t *testing.T) {
	ts := setupE2E(t)
	defer ts.mockUpstream.Close()

	clientCtx, clientCancel := context.WithCancel(context.Background())

	reqBody := `{"model":"gpt-4o","messages":[{"role":"user","content":"slow-stream"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(reqBody)).WithContext(clientCtx)
	req.Header.Set("Authorization", "Bearer sk-gw-tenant-prod-key")

	// Create pipe to simulate streaming client reading live
	pr, pw := io.Pipe()
	rec := &pipeRecorder{
		header: make(http.Header),
		writer: pw,
	}

	doneChan := make(chan struct{})
	go func() {
		ts.gatewayServer.Handler().ServeHTTP(rec, req)
		pw.Close()
		close(doneChan)
	}()

	// Read initial chunk
	scanner := bufio.NewScanner(pr)
	gotStart := false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, "start") {
			gotStart = true
			break
		}
	}

	if !gotStart {
		t.Fatal("did not receive initial chunk before cancelling")
	}

	// Cancel downstream client context
	clientCancel()
	pr.Close()

	select {
	case <-doneChan:
		// Pipeline stopped
	case <-time.After(2 * time.Second):
		t.Fatal("pipeline did not terminate after client context cancellation")
	}

	// Check that upstream received the cancellation
	time.Sleep(100 * time.Millisecond)
	if atomic.LoadInt32(&ts.upstreamCancelled) != 1 {
		t.Fatal("upstream provider did not receive cancellation upon client disconnect")
	}
}

// pipeRecorder allows streaming testing with live read/cancel.
type pipeRecorder struct {
	header http.Header
	writer *io.PipeWriter
	code   int
}

func (p *pipeRecorder) Header() http.Header {
	return p.header
}

func (p *pipeRecorder) Write(b []byte) (int, error) {
	return p.writer.Write(b)
}

func (p *pipeRecorder) WriteHeader(statusCode int) {
	p.code = statusCode
}

func (p *pipeRecorder) Flush() {}
