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

	"github.com/company/ai-gateway/internal/config"
	"github.com/company/ai-gateway/internal/model"
	"github.com/company/ai-gateway/internal/server"
)

type layer3TestSetup struct {
	mockUpstream       *httptest.Server
	gatewayServer      *server.Server
	lastUpstreamPrompt string
	upstreamCalls      int32
	upstreamCancelled  int32
	mu                 sync.Mutex
}

func setupLayer3E2E(t *testing.T) *layer3TestSetup {
	ts := &layer3TestSetup{}

	ts.mockUpstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&ts.upstreamCalls, 1)

		bodyBytes, _ := io.ReadAll(r.Body)
		var chatReq model.CanonicalChatRequest
		_ = json.Unmarshal(bodyBytes, &chatReq)

		ts.mu.Lock()
		if len(chatReq.Messages) > 0 {
			ts.lastUpstreamPrompt = chatReq.Messages[0].ContentString()
		}
		ts.mu.Unlock()

		// If caller requests a delay (for concurrency testing)
		if strings.Contains(ts.lastUpstreamPrompt, "delay_request") {
			time.Sleep(150 * time.Millisecond)
		}

		if chatReq.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)

			// If streaming with split credit card
			if strings.Contains(ts.lastUpstreamPrompt, "stream_leak_card") {
				// Stream credit card 4242-4242-4242-4242 split across chunks
				chunks := []string{
					"data: {\"choices\":[{\"delta\":{\"content\":\"Account details: card=\"}}]}\n\n",
					"data: {\"choices\":[{\"delta\":{\"content\":\"4242-\"}}]}\n\n",
					"data: {\"choices\":[{\"delta\":{\"content\":\"4242-\"}}]}\n\n",
					"data: {\"choices\":[{\"delta\":{\"content\":\"4242-\"}}]}\n\n",
					"data: {\"choices\":[{\"delta\":{\"content\":\"4242\"}}]}\n\n",
					"data: [DONE]\n\n",
				}
				for _, ch := range chunks {
					w.Write([]byte(ch))
					flusher.Flush()
					time.Sleep(10 * time.Millisecond)
				}
				return
			}

			// Clean stream
			w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"Hello from streaming\"}}]}\n\n"))
			flusher.Flush()
			w.Write([]byte("data: [DONE]\n\n"))
			flusher.Flush()
			return
		}

		// Non-streaming response
		resp := model.CanonicalChatResponse{
			ID:      "chatcmpl-layer3",
			Object:  "chat.completion",
			Created: time.Now().Unix(),
			Model:   chatReq.Model,
			Choices: []model.ChatChoice{
				{
					Index: 0,
					Message: model.ChatMessage{
						Role:    "assistant",
						Content: "Processed: " + ts.lastUpstreamPrompt,
					},
				},
			},
			Usage: &model.UsageInfo{
				PromptTokens:     10,
				CompletionTokens: 10,
				TotalTokens:      20,
			},
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(resp)
	}))

	yamlConfig := fmt.Sprintf(`
server:
  host: "127.0.0.1"
  port: 8080
upstreams:
  - id: "primary-up"
    provider: "openai"
    endpoint_url: "%s"
    api_key: "sk-upstream-secret"
    timeout_seconds: 5
routes:
  - alias: "chat-model"
    primary_upstream: "primary-up"
    model_name: "gpt-4o"
policies:
  - id: "block-ssn-policy"
    name: "block-ssn-policy"
    action: "BLOCK"
    patterns:
      - "ssn"
  - id: "mask-ssn-policy"
    name: "mask-ssn-policy"
    action: "MASK"
    patterns:
      - "ssn"
  - id: "block-card-policy"
    name: "block-card-policy"
    action: "BLOCK"
    patterns:
      - "credit_card"
tenants:
  # Tenant with BLOCK SSN policy
  - id: "tenant-block-pii"
    name: "Tenant Block PII"
    tier: "production"
    api_keys:
      - "sk-gw-block-key"
    allowed_routes:
      - "chat-model"
    policy_bindings:
      - "block-ssn-policy"
      - "block-card-policy"

  # Tenant with MASK SSN policy
  - id: "tenant-mask-pii"
    name: "Tenant Mask PII"
    tier: "production"
    api_keys:
      - "sk-gw-mask-key"
    allowed_routes:
      - "chat-model"
    policy_bindings:
      - "mask-ssn-policy"

  # Tenant with Strict RPM Limit (2 req/min)
  - id: "tenant-rpm-limited"
    name: "Tenant RPM Limited"
    tier: "sandbox"
    api_keys:
      - "sk-gw-rpm-key"
    allowed_routes:
      - "chat-model"
    rate_limits:
      requests_per_minute: 2

  # Tenant with Strict TPM Limit (600 TPM)
  - id: "tenant-tpm-limited"
    name: "Tenant TPM Limited"
    tier: "sandbox"
    api_keys:
      - "sk-gw-tpm-key"
    allowed_routes:
      - "chat-model"
    rate_limits:
      tokens_per_minute: 600

  # Tenant with Concurrency Limit (1 active request)
  - id: "tenant-conc-limited"
    name: "Tenant Concurrency Limited"
    tier: "sandbox"
    api_keys:
      - "sk-gw-conc-key"
    allowed_routes:
      - "chat-model"
    rate_limits:
      max_concurrent: 1
`, ts.mockUpstream.URL)

	cfg, err := config.ParseConfig(yamlConfig)
	if err != nil {
		t.Fatalf("failed to parse test config: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts.gatewayServer = server.NewServer(cfg, logger)

	return ts
}

// 1. Test request with SSN blocked with HTTP 400.
func TestE2ESSNBlockedWithHTTP400(t *testing.T) {
	ts := setupLayer3E2E(t)
	defer ts.mockUpstream.Close()

	body := `{"model":"chat-model","messages":[{"role":"user","content":"My social security is 123-45-6789"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer sk-gw-block-key")
	rec := httptest.NewRecorder()

	ts.gatewayServer.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected HTTP 400 Bad Request, got: %d, body: %s", rec.Code, rec.Body.String())
	}

	var errResp model.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("failed to unmarshal error response: %v", err)
	}

	if errResp.Error.Code != model.ErrCodePolicyViolation {
		t.Fatalf("expected error code %s, got: %s", model.ErrCodePolicyViolation, errResp.Error.Code)
	}

	// Verify upstream was NEVER called
	if atomic.LoadInt32(&ts.upstreamCalls) != 0 {
		t.Fatalf("expected 0 upstream calls for blocked prompt, got %d", ts.upstreamCalls)
	}
}

// 2. Test request with PII redacted in prompt before hitting upstream.
func TestE2EPIIRedactedInPromptBeforeUpstream(t *testing.T) {
	ts := setupLayer3E2E(t)
	defer ts.mockUpstream.Close()

	body := `{"model":"chat-model","messages":[{"role":"user","content":"Customer SSN 123-45-6789 verified."}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer sk-gw-mask-key")
	rec := httptest.NewRecorder()

	ts.gatewayServer.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200 OK, got: %d, body: %s", rec.Code, rec.Body.String())
	}

	// Verify upstream received the redacted prompt
	ts.mu.Lock()
	receivedPrompt := ts.lastUpstreamPrompt
	ts.mu.Unlock()

	expectedPrompt := "Customer SSN [REDACTED:mask-ssn-policy] verified."
	if receivedPrompt != expectedPrompt {
		t.Fatalf("expected upstream to receive redacted prompt '%s', but got '%s'", expectedPrompt, receivedPrompt)
	}

	if strings.Contains(receivedPrompt, "123-45-6789") {
		t.Fatalf("CRITICAL: raw SSN leaked to upstream provider: %s", receivedPrompt)
	}
}

// 3. Test rate limit RPM and TPM exhaustion returning 429 with Retry-After.
func TestE2ERateLimitRPMExhaustion(t *testing.T) {
	ts := setupLayer3E2E(t)
	defer ts.mockUpstream.Close()

	body := `{"model":"chat-model","messages":[{"role":"user","content":"ping"}]}`

	// Request 1: OK
	req1 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req1.Header.Set("Authorization", "Bearer sk-gw-rpm-key")
	rec1 := httptest.NewRecorder()
	ts.gatewayServer.Handler().ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("request 1 expected 200, got %d", rec1.Code)
	}

	// Request 2: OK
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req2.Header.Set("Authorization", "Bearer sk-gw-rpm-key")
	rec2 := httptest.NewRecorder()
	ts.gatewayServer.Handler().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("request 2 expected 200, got %d", rec2.Code)
	}

	// Request 3: 429 RATE_LIMIT_EXCEEDED with Retry-After header
	req3 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req3.Header.Set("Authorization", "Bearer sk-gw-rpm-key")
	rec3 := httptest.NewRecorder()
	ts.gatewayServer.Handler().ServeHTTP(rec3, req3)

	if rec3.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 Too Many Requests, got %d", rec3.Code)
	}

	retryAfterHeader := rec3.Header().Get("Retry-After")
	if retryAfterHeader == "" {
		t.Fatal("expected Retry-After header to be present on 429 response")
	}

	var errResp model.ErrorResponse
	json.Unmarshal(rec3.Body.Bytes(), &errResp)
	if errResp.Error.Code != model.ErrCodeRateLimitExceeded {
		t.Fatalf("expected error code %s, got: %s", model.ErrCodeRateLimitExceeded, errResp.Error.Code)
	}
}

func TestE2ERateLimitTPMExhaustion(t *testing.T) {
	ts := setupLayer3E2E(t)
	defer ts.mockUpstream.Close()

	// Tenant has 600 TPM.
	// Request 1: prompt has len 40 (10 tokens) + max_tokens 100 = 110 est tokens. Fits within 600 TPM.
	maxTok := 100
	chatReq1 := model.CanonicalChatRequest{
		Model: "chat-model",
		Messages: []model.ChatMessage{
			{Role: "user", Content: "Hello world 12345678901234567890"},
		},
		MaxTokens: &maxTok,
	}
	body1, _ := json.Marshal(chatReq1)
	req1 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body1))
	req1.Header.Set("Authorization", "Bearer sk-gw-tpm-key")
	rec1 := httptest.NewRecorder()
	ts.gatewayServer.Handler().ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("request 1 expected 200, got %d: %s", rec1.Code, rec1.Body.String())
	}

	// Request 2: prompt has len 800 (200 tokens) + max_tokens 450 = 650 est tokens.
	// 650 > 600 TPM -> Exceeds TPM!
	largePrompt := strings.Repeat("A", 800)
	maxTokLarge := 450
	chatReq2 := model.CanonicalChatRequest{
		Model: "chat-model",
		Messages: []model.ChatMessage{
			{Role: "user", Content: largePrompt},
		},
		MaxTokens: &maxTokLarge,
	}
	body2, _ := json.Marshal(chatReq2)
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body2))
	req2.Header.Set("Authorization", "Bearer sk-gw-tpm-key")
	rec2 := httptest.NewRecorder()
	ts.gatewayServer.Handler().ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 Too Many Requests for TPM exhaustion, got %d: %s", rec2.Code, rec2.Body.String())
	}

	retryAfterHeader := rec2.Header().Get("Retry-After")
	if retryAfterHeader == "" {
		t.Fatal("expected Retry-After header on TPM rate limit rejection")
	}
}

// 4. Test concurrency limit rejection.
func TestE2EConcurrencyLimitRejection(t *testing.T) {
	ts := setupLayer3E2E(t)
	defer ts.mockUpstream.Close()

	// Tenant has max_concurrent: 1
	bodySlow := `{"model":"chat-model","messages":[{"role":"user","content":"delay_request 1"}]}`

	started := make(chan struct{})
	doneFirst := make(chan struct{})

	var code1 int
	go func() {
		req1 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(bodySlow))
		req1.Header.Set("Authorization", "Bearer sk-gw-conc-key")
		rec1 := httptest.NewRecorder()
		close(started)
		ts.gatewayServer.Handler().ServeHTTP(rec1, req1)
		code1 = rec1.Code
		close(doneFirst)
	}()

	<-started
	// Wait a moment so request 1 is actively executing in mock upstream
	time.Sleep(20 * time.Millisecond)

	// Send request 2 concurrently while request 1 is active
	bodyFast := `{"model":"chat-model","messages":[{"role":"user","content":"immediate request"}]}`
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(bodyFast))
	req2.Header.Set("Authorization", "Bearer sk-gw-conc-key")
	rec2 := httptest.NewRecorder()
	ts.gatewayServer.Handler().ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("expected concurrent request 2 to be rejected with 429, got %d: %s", rec2.Code, rec2.Body.String())
	}

	var errResp model.ErrorResponse
	json.Unmarshal(rec2.Body.Bytes(), &errResp)
	if errResp.Error.Code != model.ErrCodeConcurrencyLimitExceeded {
		t.Fatalf("expected %s, got: %s", model.ErrCodeConcurrencyLimitExceeded, errResp.Error.Code)
	}

	// Wait for request 1 to complete
	<-doneFirst
	if code1 != http.StatusOK {
		t.Fatalf("expected request 1 to finish with 200, got: %d", code1)
	}

	// Send request 3 after request 1 has freed the semaphore slot
	req3 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(bodyFast))
	req3.Header.Set("Authorization", "Bearer sk-gw-conc-key")
	rec3 := httptest.NewRecorder()
	ts.gatewayServer.Handler().ServeHTTP(rec3, req3)

	if rec3.Code != http.StatusOK {
		t.Fatalf("expected request 3 to succeed after slot released, got %d", rec3.Code)
	}
}

// 5. Test streaming SSE DLP violation mid-stream abort.
func TestE2EStreamingSSEDLPViolationMidStreamAbort(t *testing.T) {
	ts := setupLayer3E2E(t)
	defer ts.mockUpstream.Close()

	// Upstream will emit "stream_leak_card" with split credit card
	body := `{"model":"chat-model","messages":[{"role":"user","content":"stream_leak_card"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer sk-gw-block-key")
	rec := httptest.NewRecorder()

	ts.gatewayServer.Handler().ServeHTTP(rec, req)

	streamBody := rec.Body.String()

	// Verify SSE error event was emitted to client
	if !strings.Contains(streamBody, "POLICY_VIOLATION") {
		t.Fatalf("expected stream to contain POLICY_VIOLATION error event, got: %s", streamBody)
	}

	// CRITICAL SECURITY ASSERTION:
	// Verify the credit card number was NEVER leaked to client
	if strings.Contains(streamBody, "4242-4242-4242-4242") {
		t.Fatalf("CRITICAL SECURITY VIOLATION: full credit card leaked to client stream: %s", streamBody)
	}
}
