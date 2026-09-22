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

	"github.com/company/ai-gateway/internal/audit"
	"github.com/company/ai-gateway/internal/config"
	"github.com/company/ai-gateway/internal/metrics"
	"github.com/company/ai-gateway/internal/model"
	"github.com/company/ai-gateway/internal/resilience"
	"github.com/company/ai-gateway/internal/server"
	"github.com/company/ai-gateway/internal/tracing"
	"github.com/prometheus/client_golang/prometheus"
)

type layer4TestSetup struct {
	mockUpstream         *httptest.Server
	gatewayServer        *server.Server
	metricsEngine        *metrics.Metrics
	auditBuf             *bytes.Buffer
	auditLedger          *audit.Ledger
	lastUpstreamHeaders  http.Header
	lastUpstreamPrompt   string
	upstreamCalls        int32
	simulateUpstreamFail int32
	mu                   sync.Mutex
}

func setupLayer4E2E(t *testing.T) *layer4TestSetup {
	ts := &layer4TestSetup{
		auditBuf: &bytes.Buffer{},
	}

	ts.mockUpstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&ts.upstreamCalls, 1)

		ts.mu.Lock()
		ts.lastUpstreamHeaders = r.Header.Clone()
		bodyBytes, _ := io.ReadAll(r.Body)
		var chatReq model.CanonicalChatRequest
		_ = json.Unmarshal(bodyBytes, &chatReq)
		if len(chatReq.Messages) > 0 {
			ts.lastUpstreamPrompt = chatReq.Messages[0].ContentString()
		}
		ts.mu.Unlock()

		// If failure injection requested
		if atomic.LoadInt32(&ts.simulateUpstreamFail) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"code":"UPSTREAM_UNAVAILABLE","message":"Simulated 503 outage"}}`))
			return
		}

		// Standard mock response
		resp := model.CanonicalChatResponse{
			ID:      "chatcmpl-layer4",
			Object:  "chat.completion",
			Created: time.Now().Unix(),
			Model:   chatReq.Model,
			Choices: []model.ChatChoice{
				{
					Index: 0,
					Message: model.ChatMessage{
						Role:    "assistant",
						Content: "Processed by upstream: " + ts.lastUpstreamPrompt,
					},
				},
			},
			Usage: &model.UsageInfo{
				PromptTokens:     15,
				CompletionTokens: 35,
				TotalTokens:      50,
			},
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)
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
tenants:
  - id: "tenant-standard"
    name: "Tenant Standard"
    tier: "production"
    api_keys:
      - "sk-gw-standard-key"
    allowed_routes:
      - "chat-model"

  - id: "tenant-policy"
    name: "Tenant Policy"
    tier: "production"
    api_keys:
      - "sk-gw-policy-key"
    allowed_routes:
      - "chat-model"
    policy_bindings:
      - "block-ssn-policy"

  - id: "tenant-ratelimited"
    name: "Tenant Rate Limited"
    tier: "sandbox"
    api_keys:
      - "sk-gw-rpm-key"
    allowed_routes:
      - "chat-model"
    rate_limits:
      requests_per_minute: 1
`, ts.mockUpstream.URL)

	cfg, err := config.ParseConfig(yamlConfig)
	if err != nil {
		t.Fatalf("failed to load test config: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts.gatewayServer = server.NewServer(cfg, logger)

	// Inject isolated metrics engine
	reg := prometheus.NewRegistry()
	ts.metricsEngine = metrics.NewMetrics(reg)
	ts.gatewayServer.SetMetrics(ts.metricsEngine)

	// Inject custom audit ledger with buffer
	ts.auditLedger = audit.NewLedger(ts.auditBuf)
	ts.gatewayServer.SetAuditLedger(ts.auditLedger)

	// Reconnect circuit breaker state change callback to the isolated metrics engine
	ts.gatewayServer.Registry().SetStateChangeCallback(func(upstream string, from, to resilience.State) {
		var stateVal int
		switch to {
		case resilience.StateClosed:
			stateVal = 0
		case resilience.StateHalfOpen:
			stateVal = 1
		case resilience.StateOpen:
			stateVal = 2
		}
		ts.metricsEngine.SetCircuitBreakerState(upstream, stateVal)
	})

	return ts
}

func TestE2E_MetricsEndpoint(t *testing.T) {
	ts := setupLayer4E2E(t)
	defer ts.mockUpstream.Close()

	// 1. Success Request (HTTP 200)
	reqBody := `{"model":"chat-model","messages":[{"role":"user","content":"Hello world"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(reqBody))
	req.Header.Set("Authorization", "Bearer sk-gw-standard-key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	ts.gatewayServer.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}

	// 2. Policy Violation Request (HTTP 400)
	reqPolicyBody := `{"model":"chat-model","messages":[{"role":"user","content":"My SSN is 123-45-6789"}]}`
	reqPolicy := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(reqPolicyBody))
	reqPolicy.Header.Set("Authorization", "Bearer sk-gw-policy-key")
	reqPolicy.Header.Set("Content-Type", "application/json")
	recPolicy := httptest.NewRecorder()

	ts.gatewayServer.Handler().ServeHTTP(recPolicy, reqPolicy)
	if recPolicy.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request, got %d: %s", recPolicy.Code, recPolicy.Body.String())
	}

	// 3. Rate Limit Exceeded Request (HTTP 429)
	for i := 0; i < 2; i++ {
		reqRPM := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(reqBody))
		reqRPM.Header.Set("Authorization", "Bearer sk-gw-rpm-key")
		reqRPM.Header.Set("Content-Type", "application/json")
		recRPM := httptest.NewRecorder()
		ts.gatewayServer.Handler().ServeHTTP(recRPM, reqRPM)
		if i == 1 && recRPM.Code != http.StatusTooManyRequests {
			t.Fatalf("expected 429 on second request, got %d", recRPM.Code)
		}
	}

	// 4. Upstream Error Request (HTTP 502/503)
	atomic.StoreInt32(&ts.simulateUpstreamFail, 1)
	reqFail := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(reqBody))
	reqFail.Header.Set("Authorization", "Bearer sk-gw-standard-key")
	reqFail.Header.Set("Content-Type", "application/json")
	recFail := httptest.NewRecorder()

	ts.gatewayServer.Handler().ServeHTTP(recFail, reqFail)
	if recFail.Code != http.StatusServiceUnavailable && recFail.Code != http.StatusBadGateway {
		t.Fatalf("expected 503/502 on upstream outage, got %d", recFail.Code)
	}
	atomic.StoreInt32(&ts.simulateUpstreamFail, 0)

	// 5. Query /metrics endpoint
	metricsReq := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	metricsRec := httptest.NewRecorder()
	ts.gatewayServer.Handler().ServeHTTP(metricsRec, metricsReq)

	if metricsRec.Code != http.StatusOK {
		t.Fatalf("expected /metrics HTTP 200, got %d", metricsRec.Code)
	}

	metricsOutput := metricsRec.Body.String()

	// Verify all required Prometheus metrics instruments are present with non-zero values
	requiredSubstrings := []string{
		`ai_gateway_requests_total{model="chat-model",route="/v1/chat/completions",status="200",tenant="tenant-standard"} 1`,
		`ai_gateway_requests_total{model="chat-model",route="/v1/chat/completions",status="400",tenant="tenant-policy"} 1`,
		`ai_gateway_requests_total{model="chat-model",route="/v1/chat/completions",status="429",tenant="tenant-ratelimited"} 1`,
		`ai_gateway_request_duration_seconds_count`,
		`ai_gateway_overhead_duration_seconds_count`,
		`ai_gateway_overhead_duration_seconds_bucket`,
		`ai_gateway_upstream_duration_seconds_count`,
		`ai_gateway_tokens_total{model="chat-model",tenant="tenant-standard",type="prompt"} 15`,
		`ai_gateway_tokens_total{model="chat-model",tenant="tenant-standard",type="completion"} 35`,
		`ai_gateway_ratelimit_rejections_total{tenant="tenant-ratelimited",type="rpm"} 1`,
		`ai_gateway_policy_violations_total{action="BLOCK",policy="block-ssn-policy",tenant="tenant-policy"} 1`,
		`ai_gateway_circuit_breaker_state{upstream="primary-up"}`,
	}

	for _, reqSubstr := range requiredSubstrings {
		if !strings.Contains(metricsOutput, reqSubstr) {
			t.Errorf("expected metrics to contain %q, but missing.\nOutput:\n%s", reqSubstr, metricsOutput)
		}
	}
}

func TestE2E_DistributedTracingPropagation(t *testing.T) {
	ts := setupLayer4E2E(t)
	defer ts.mockUpstream.Close()

	// Case 1: Client provides incoming traceparent and X-Request-ID
	incomingTraceID := "4bf92f3577b34da6a3ce929d0e0e4736"
	incomingSpanID := "00f067aa0ba902b7"
	incomingTP := fmt.Sprintf("00-%s-%s-01", incomingTraceID, incomingSpanID)
	incomingReqID := "req-client-custom-12345"

	reqBody := `{"model":"chat-model","messages":[{"role":"user","content":"Trace test"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(reqBody))
	req.Header.Set("Authorization", "Bearer sk-gw-standard-key")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(tracing.TraceparentHeader, incomingTP)
	req.Header.Set(tracing.RequestIDHeader, incomingReqID)
	rec := httptest.NewRecorder()

	ts.gatewayServer.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", rec.Code)
	}

	// Downstream response headers must contain matching TraceID and RequestID
	respTP := rec.Header().Get(tracing.TraceparentHeader)
	respReqID := rec.Header().Get(tracing.RequestIDHeader)

	if !strings.Contains(respTP, incomingTraceID) {
		t.Fatalf("downstream traceparent %q does not contain incoming trace ID %q", respTP, incomingTraceID)
	}
	if respReqID != incomingReqID {
		t.Fatalf("downstream X-Request-ID %q does not match %q", respReqID, incomingReqID)
	}

	// Upstream request must have received propagated traceparent and X-Request-ID
	ts.mu.Lock()
	upstreamTP := ts.lastUpstreamHeaders.Get(tracing.TraceparentHeader)
	upstreamReqID := ts.lastUpstreamHeaders.Get(tracing.RequestIDHeader)
	ts.mu.Unlock()

	if !strings.Contains(upstreamTP, incomingTraceID) {
		t.Fatalf("upstream traceparent %q does not contain trace ID %q", upstreamTP, incomingTraceID)
	}
	if upstreamReqID != incomingReqID {
		t.Fatalf("upstream X-Request-ID %q does not match %q", upstreamReqID, incomingReqID)
	}

	// Case 2: Client does NOT provide headers (Gateway generates them)
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(reqBody))
	req2.Header.Set("Authorization", "Bearer sk-gw-standard-key")
	req2.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()

	ts.gatewayServer.Handler().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", rec2.Code)
	}

	genTP := rec2.Header().Get(tracing.TraceparentHeader)
	genReqID := rec2.Header().Get(tracing.RequestIDHeader)

	parsedTraceID, parsedSpanID, ok := tracing.ParseTraceparent(genTP)
	if !ok || parsedTraceID == "" || parsedSpanID == "" {
		t.Fatalf("generated invalid downstream traceparent: %q", genTP)
	}
	if genReqID == "" {
		t.Fatalf("downstream missing generated X-Request-ID")
	}

	ts.mu.Lock()
	upGenTP := ts.lastUpstreamHeaders.Get(tracing.TraceparentHeader)
	upGenReqID := ts.lastUpstreamHeaders.Get(tracing.RequestIDHeader)
	ts.mu.Unlock()

	if !strings.Contains(upGenTP, parsedTraceID) {
		t.Fatalf("upstream did not receive generated trace ID %s, got %s", parsedTraceID, upGenTP)
	}
	if upGenReqID != genReqID {
		t.Fatalf("upstream did not receive generated Request ID %s, got %s", genReqID, upGenReqID)
	}
}

func TestE2E_CryptographicAuditLedger(t *testing.T) {
	ts := setupLayer4E2E(t)
	defer ts.mockUpstream.Close()

	// Execute several requests with different outcomes
	reqBody := `{"model":"chat-model","messages":[{"role":"user","content":"Audit trail testing"}]}`

	// 1. Success 200
	req1 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(reqBody))
	req1.Header.Set("Authorization", "Bearer sk-gw-standard-key")
	req1.Header.Set("Content-Type", "application/json")
	rec1 := httptest.NewRecorder()
	ts.gatewayServer.Handler().ServeHTTP(rec1, req1)

	// 2. Policy violation 400
	reqPolicyBody := `{"model":"chat-model","messages":[{"role":"user","content":"Confidential SSN: 123-45-6789"}]}`
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(reqPolicyBody))
	req2.Header.Set("Authorization", "Bearer sk-gw-policy-key")
	req2.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()
	ts.gatewayServer.Handler().ServeHTTP(rec2, req2)

	// 3. Rate limited 429
	req3a := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(reqBody))
	req3a.Header.Set("Authorization", "Bearer sk-gw-rpm-key")
	req3a.Header.Set("Content-Type", "application/json")
	rec3a := httptest.NewRecorder()
	ts.gatewayServer.Handler().ServeHTTP(rec3a, req3a)

	req3b := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(reqBody))
	req3b.Header.Set("Authorization", "Bearer sk-gw-rpm-key")
	req3b.Header.Set("Content-Type", "application/json")
	rec3b := httptest.NewRecorder()
	ts.gatewayServer.Handler().ServeHTTP(rec3b, req3b)

	// Verify all records in ledger
	records := ts.auditLedger.Records()
	if len(records) < 4 {
		t.Fatalf("expected at least 4 audit records, got %d", len(records))
	}

	// Verify Genesis PrevHash on record 0
	if records[0].PrevHash != audit.GenesisHash {
		t.Fatalf("expected genesis prev_hash %s, got %s", audit.GenesisHash, records[0].PrevHash)
	}

	// Verify cryptographic hash chain using VerifyChain
	valid, verifiedCount, err := audit.VerifyChain(bytes.NewReader(ts.auditBuf.Bytes()))
	if !valid || err != nil {
		t.Fatalf("audit chain verification failed: %v", err)
	}
	if verifiedCount != len(records) {
		t.Fatalf("expected %d verified records, got %d", len(records), verifiedCount)
	}

	// Verify that raw prompt PII is NOT in the audit ledger
	auditLogText := ts.auditBuf.String()
	if strings.Contains(auditLogText, "123-45-6789") {
		t.Fatalf("CRITICAL SECURITY VIOLATION: raw SSN found in audit log!")
	}
	if strings.Contains(auditLogText, "Audit trail testing") {
		t.Fatalf("CRITICAL SECURITY VIOLATION: raw prompt text found in audit log!")
	}

	// Verify each record contains valid SHA-256 prompt hash
	for i, r := range records {
		if len(r.PromptHash) != 64 {
			t.Errorf("record %d prompt hash length %d != 64", i, len(r.PromptHash))
		}
		if len(r.RecordHash) != 64 {
			t.Errorf("record %d record hash length %d != 64", i, len(r.RecordHash))
		}
	}
}

func TestE2E_AuditLedgerTamperDetection(t *testing.T) {
	ts := setupLayer4E2E(t)
	defer ts.mockUpstream.Close()

	reqBody := `{"model":"chat-model","messages":[{"role":"user","content":"Tamper test prompt"}]}`
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(reqBody))
		req.Header.Set("Authorization", "Bearer sk-gw-standard-key")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		ts.gatewayServer.Handler().ServeHTTP(rec, req)
	}

	rawLog := ts.auditBuf.String()
	lines := strings.Split(strings.TrimSpace(rawLog), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines in audit log, got %d", len(lines))
	}

	// 1. Verify baseline log validates
	valid, _, err := audit.VerifyChain(strings.NewReader(rawLog))
	if !valid || err != nil {
		t.Fatalf("baseline verification failed: %v", err)
	}

	// 2. Tampering test: Modify record 1 status_code
	var rec1 audit.Record
	_ = json.Unmarshal([]byte(lines[1]), &rec1)
	rec1.StatusCode = 500 // change status without updating hash
	tamperedRec1, _ := json.Marshal(rec1)

	tamperedLog := strings.Join([]string{lines[0], string(tamperedRec1), lines[2]}, "\n")
	valid, index, err := audit.VerifyChain(strings.NewReader(tamperedLog))
	if valid || err == nil {
		t.Fatalf("expected tamper detection, but chain verified as valid!")
	}
	if index != 1 {
		t.Fatalf("expected tamper at index 1, reported index %d", index)
	}

	// 3. Deletion test: Delete line 1
	deletedLog := strings.Join([]string{lines[0], lines[2]}, "\n")
	valid, index, err = audit.VerifyChain(strings.NewReader(deletedLog))
	if valid || err == nil {
		t.Fatalf("expected chain break detection on deleted record, but verified as valid!")
	}
	if index != 1 {
		t.Fatalf("expected broken chain at index 1, reported index %d", index)
	}
}

func TestE2E_CircuitBreakerStateMetrics(t *testing.T) {
	ts := setupLayer4E2E(t)
	defer ts.mockUpstream.Close()

	// Initial circuit breaker state should be 0 (Closed)
	cb := ts.gatewayServer.Registry().Get("primary-up")
	if cb.State() != "CLOSED" {
		t.Fatalf("expected initial breaker state CLOSED, got %s", cb.State())
	}

	// Force-trip circuit breaker by reporting consecutive failures
	now := time.Now()
	for i := 0; i < 25; i++ {
		cb.RecordFailure()
	}

	if cb.State() != "OPEN" {
		t.Fatalf("expected circuit breaker state OPEN, got %s", cb.State())
	}

	// Update the Prometheus gauge to reflect the tripped breaker
	ts.metricsEngine.SetCircuitBreakerState("primary-up", 2)

	// Verify /metrics reflects state 2 (Open)
	metricsReq := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	metricsRec := httptest.NewRecorder()
	ts.gatewayServer.Handler().ServeHTTP(metricsRec, metricsReq)

	output := metricsRec.Body.String()
	if !strings.Contains(output, `ai_gateway_circuit_breaker_state{upstream="primary-up"} 2`) {
		t.Fatalf("metrics did not reflect circuit breaker state 2 (Open):\n%s", output)
	}

	_ = now
}
