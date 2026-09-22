package limiter

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/company/ai-gateway/internal/model"
	"github.com/company/ai-gateway/internal/pipeline"
)

func TestLimiterRPMSlidingWindow(t *testing.T) {
	l := NewLimiter()
	tenantID := "tenant-test-rpm"
	limits := model.RateLimits{
		RequestsPerMinute: 3,
	}

	baseTime := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)

	// 1. First 3 requests at baseTime should succeed
	for i := 0; i < 3; i++ {
		res, retryAfter, err := l.AcquireAt(tenantID, limits, 100, baseTime.Add(time.Duration(i)*time.Second))
		if err != nil {
			t.Fatalf("request %d expected success, got: %v", i, err)
		}
		if retryAfter != 0 {
			t.Fatalf("expected retryAfter 0, got %d", retryAfter)
		}
		res.Reconcile(100)
	}

	// 2. 4th request at baseTime + 10s should be rejected with RATE_LIMIT_EXCEEDED
	_, retryAfter, err := l.AcquireAt(tenantID, limits, 100, baseTime.Add(10*time.Second))
	if err == nil {
		t.Fatal("expected 4th request to be rejected, but succeeded")
	}

	var gwErr *model.GatewayError
	if !errors.As(err, &gwErr) || gwErr.StatusCode != http.StatusTooManyRequests || gwErr.Code != model.ErrCodeRateLimitExceeded {
		t.Fatalf("expected 429 RATE_LIMIT_EXCEEDED, got: %v", err)
	}

	// Oldest request was at baseTime + 0s. At baseTime + 10s, retryAfter should be (60 - 10) = 50s.
	if retryAfter != 50 {
		t.Fatalf("expected retryAfter 50s, got %d", retryAfter)
	}

	// 3. Request after window slides (baseTime + 61s): oldest request has rolled out
	res, retryAfter, err := l.AcquireAt(tenantID, limits, 100, baseTime.Add(61*time.Second))
	if err != nil {
		t.Fatalf("expected request after window slide to succeed, got: %v", err)
	}
	res.Reconcile(100)
}

func TestLimiterTPMTwoPhaseReservationAndReconciliation(t *testing.T) {
	l := NewLimiter()
	tenantID := "tenant-test-tpm"
	limits := model.RateLimits{
		TokensPerMinute: 1000,
	}

	baseTime := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)

	// Phase 1: Reserve 600 estimated tokens (out of 1000)
	res1, retryAfter, err := l.AcquireAt(tenantID, limits, 600, baseTime)
	if err != nil {
		t.Fatalf("expected acquire 1 to succeed, got: %v", err)
	}
	if l.ReservedTokens(tenantID) != 600 {
		t.Fatalf("expected 600 reserved tokens, got %d", l.ReservedTokens(tenantID))
	}

	// Second reservation of 500 tokens should fail because 600 + 500 = 1100 > 1000
	_, retryAfter, err = l.AcquireAt(tenantID, limits, 500, baseTime.Add(1*time.Second))
	if err == nil {
		t.Fatal("expected second acquire to exceed TPM limit, but succeeded")
	}
	if retryAfter < 1 {
		t.Fatalf("expected positive retryAfter, got %d", retryAfter)
	}

	// Phase 2: Post-dispatch reconciliation with actual usage < estimated:
	// Actual tokens used = 200 (refunds 400 tokens)
	res1.ReconcileAt(200, baseTime.Add(1*time.Second))
	if l.ReservedTokens(tenantID) != 0 {
		t.Fatalf("expected 0 reserved tokens after reconcile, got %d", l.ReservedTokens(tenantID))
	}

	// Now remaining budget is 1000 - 200 = 800.
	// We should be able to acquire 500 tokens now!
	res2, _, err := l.AcquireAt(tenantID, limits, 500, baseTime.Add(2*time.Second))
	if err != nil {
		t.Fatalf("expected acquire after refund to succeed, got: %v", err)
	}

	// Phase 2: Post-dispatch reconciliation where actual > estimated:
	// Estimated was 500, but actual was 700
	res2.ReconcileAt(700, baseTime.Add(2*time.Second))

	// Total used in window is now 200 + 700 = 900.
	// Next reservation of 200 tokens should exceed 1000 (900 + 200 = 1100 > 1000)
	_, _, err = l.AcquireAt(tenantID, limits, 200, baseTime.Add(3*time.Second))
	if err == nil {
		t.Fatal("expected acquire of 200 tokens to exceed 1000 TPM limit, but succeeded")
	}

	// But reservation of 100 tokens should succeed (900 + 100 <= 1000)
	res3, _, err := l.AcquireAt(tenantID, limits, 100, baseTime.Add(4*time.Second))
	if err != nil {
		t.Fatalf("expected acquire of 100 tokens to succeed, got: %v", err)
	}

	// Client aborts: reconcile with 0 tokens
	res3.Reconcile(0)
	if l.ReservedTokens(tenantID) != 0 {
		t.Fatalf("expected 0 reserved tokens after abort reconcile, got %d", l.ReservedTokens(tenantID))
	}
}

func TestLimiterConcurrencySemaphore(t *testing.T) {
	l := NewLimiter()
	tenantID := "tenant-test-conc"
	limits := model.RateLimits{
		MaxConcurrent: 2,
	}

	baseTime := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)

	// Acquire 2 active slots
	res1, _, err := l.AcquireAt(tenantID, limits, 10, baseTime)
	if err != nil {
		t.Fatalf("acquire 1 failed: %v", err)
	}
	res2, _, err := l.AcquireAt(tenantID, limits, 10, baseTime)
	if err != nil {
		t.Fatalf("acquire 2 failed: %v", err)
	}

	if l.ActiveConcurrent(tenantID) != 2 {
		t.Fatalf("expected 2 active concurrent, got %d", l.ActiveConcurrent(tenantID))
	}

	// 3rd concurrent request must be rejected with CONCURRENCY_LIMIT_EXCEEDED
	_, _, err = l.AcquireAt(tenantID, limits, 10, baseTime)
	if err == nil {
		t.Fatal("expected 3rd request to be rejected by concurrency limit, but succeeded")
	}

	var gwErr *model.GatewayError
	if !errors.As(err, &gwErr) || gwErr.StatusCode != http.StatusTooManyRequests || gwErr.Code != model.ErrCodeConcurrencyLimitExceeded {
		t.Fatalf("expected 429 CONCURRENCY_LIMIT_EXCEEDED, got: %v", err)
	}

	// Release 1 slot
	res1.Reconcile(10)
	if l.ActiveConcurrent(tenantID) != 1 {
		t.Fatalf("expected 1 active concurrent after release, got %d", l.ActiveConcurrent(tenantID))
	}

	// Now another request can acquire
	res3, _, err := l.AcquireAt(tenantID, limits, 10, baseTime)
	if err != nil {
		t.Fatalf("expected acquire after release to succeed, got: %v", err)
	}

	res2.Reconcile(10)
	res3.Reconcile(10)

	if l.ActiveConcurrent(tenantID) != 0 {
		t.Fatalf("expected 0 active concurrent at end, got %d", l.ActiveConcurrent(tenantID))
	}
}

func TestLimiterThreadSafetyContention(t *testing.T) {
	l := NewLimiter()
	tenantID := "tenant-concurrent"
	limits := model.RateLimits{
		RequestsPerMinute: 10000,
		TokensPerMinute:   10000000,
		MaxConcurrent:     50,
	}

	const goroutines = 100
	const iterations = 50

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				res, _, err := l.Acquire(tenantID, limits, 100)
				if err == nil {
					time.Sleep(100 * time.Microsecond)
					res.Reconcile(50)
				}
			}
		}()
	}

	wg.Wait()

	if l.ActiveConcurrent(tenantID) != 0 {
		t.Fatalf("expected active concurrency 0 after all goroutines finished, got %d", l.ActiveConcurrent(tenantID))
	}
	if l.ReservedTokens(tenantID) != 0 {
		t.Fatalf("expected reserved tokens 0 after all finished, got %d", l.ReservedTokens(tenantID))
	}
}

func TestQuotaStageTokenEstimationAndReconciliation(t *testing.T) {
	lim := NewLimiter()
	stage := NewQuotaStage(lim)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	reqCtx := pipeline.NewRequestContext(context.Background(), w, r)

	// Prompt length: 40 chars -> promptTokensEst = 10
	// max_tokens: 150
	// estTokens = 10 + 150 = 160
	maxTok := 150
	reqCtx.ChatRequest = &model.CanonicalChatRequest{
		Model: "gpt-4o",
		Messages: []model.ChatMessage{
			{Role: "user", Content: "1234567890123456789012345678901234567890"}, // 40 chars
		},
		MaxTokens: &maxTok,
	}
	reqCtx.Tenant = &model.TenantContext{
		ID: "t-stage-test",
		RateLimits: model.RateLimits{
			RequestsPerMinute: 60,
			TokensPerMinute:   1000,
			MaxConcurrent:     10,
		},
	}

	err := stage.Execute(reqCtx)
	if err != nil {
		t.Fatalf("unexpected stage error: %v", err)
	}

	if reqCtx.Reconcile == nil {
		t.Fatal("expected reqCtx.Reconcile to be populated")
	}

	if lim.ReservedTokens("t-stage-test") != 160 {
		t.Fatalf("expected 160 reserved tokens, got %d", lim.ReservedTokens("t-stage-test"))
	}
	if lim.ActiveConcurrent("t-stage-test") != 1 {
		t.Fatalf("expected 1 active concurrent, got %d", lim.ActiveConcurrent("t-stage-test"))
	}

	// Execute reconciliation with actual tokens = 80
	reqCtx.ExecuteReconcile(80)

	if lim.ReservedTokens("t-stage-test") != 0 {
		t.Fatalf("expected 0 reserved tokens after reconcile, got %d", lim.ReservedTokens("t-stage-test"))
	}
	if lim.ActiveConcurrent("t-stage-test") != 0 {
		t.Fatalf("expected 0 active concurrent after reconcile, got %d", lim.ActiveConcurrent("t-stage-test"))
	}

	// Subsequent calls to ExecuteReconcile must be no-ops (idempotent)
	reqCtx.ExecuteReconcile(80)
	if lim.ActiveConcurrent("t-stage-test") != 0 {
		t.Fatalf("expected 0 active concurrent, got %d", lim.ActiveConcurrent("t-stage-test"))
	}
}
