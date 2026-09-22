package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestMetricsRecordAndExpose(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	// Record sample telemetry
	m.RecordRequest("tenant-a", "/v1/chat/completions", "200", "gpt-4o", 0.150, 0.002)
	m.RecordUpstream("openai-direct", "gpt-4o", "200", 0.148)
	m.RecordTokens("tenant-a", "gpt-4o", 25, 60)
	m.RecordRateLimitRejection("tenant-b", "rpm")
	m.RecordRateLimitRejection("tenant-b", "tpm")
	m.RecordRateLimitRejection("tenant-b", "concurrency")
	m.RecordPolicyViolation("tenant-c", "credit_card", "BLOCK")
	m.SetCircuitBreakerState("anthropic-direct", 2)

	// Request /metrics
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d", rec.Code)
	}

	body := rec.Body.String()

	expectedSubstrings := []string{
		`ai_gateway_requests_total{model="gpt-4o",route="/v1/chat/completions",status="200",tenant="tenant-a"} 1`,
		`ai_gateway_request_duration_seconds_count{route="/v1/chat/completions",tenant="tenant-a"} 1`,
		`ai_gateway_overhead_duration_seconds_bucket{route="/v1/chat/completions",tenant="tenant-a",le="0.002"} 1`,
		`ai_gateway_upstream_duration_seconds_count{model="gpt-4o",status="200",upstream="openai-direct"} 1`,
		`ai_gateway_tokens_total{model="gpt-4o",tenant="tenant-a",type="prompt"} 25`,
		`ai_gateway_tokens_total{model="gpt-4o",tenant="tenant-a",type="completion"} 60`,
		`ai_gateway_ratelimit_rejections_total{tenant="tenant-b",type="rpm"} 1`,
		`ai_gateway_ratelimit_rejections_total{tenant="tenant-b",type="tpm"} 1`,
		`ai_gateway_ratelimit_rejections_total{tenant="tenant-b",type="concurrency"} 1`,
		`ai_gateway_policy_violations_total{action="BLOCK",policy="credit_card",tenant="tenant-c"} 1`,
		`ai_gateway_circuit_breaker_state{upstream="anthropic-direct"} 2`,
	}

	for _, substr := range expectedSubstrings {
		if !strings.Contains(body, substr) {
			t.Errorf("metrics output missing expected substring: %q\nFull output:\n%s", substr, body)
		}
	}
}
