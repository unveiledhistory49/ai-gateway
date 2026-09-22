package resilience

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestFullJitterBackoffBounds(t *testing.T) {
	baseDelay := 200 * time.Millisecond
	maxDelay := 10 * time.Second

	// Verify attempt 0: cap is 200ms * 2^0 = 200ms
	for i := 0; i < 100; i++ {
		d := FullJitterBackoff(0, baseDelay, maxDelay, nil)
		if d < 0 || d > baseDelay {
			t.Fatalf("attempt 0 backoff out of range [0, %v]: got %v", baseDelay, d)
		}
	}

	// Verify attempt 1: cap is 200ms * 2^1 = 400ms
	for i := 0; i < 100; i++ {
		d := FullJitterBackoff(1, baseDelay, maxDelay, nil)
		if d < 0 || d > 400*time.Millisecond {
			t.Fatalf("attempt 1 backoff out of range [0, 400ms]: got %v", d)
		}
	}

	// Verify attempt 2: cap is 200ms * 2^2 = 800ms
	for i := 0; i < 100; i++ {
		d := FullJitterBackoff(2, baseDelay, maxDelay, nil)
		if d < 0 || d > 800*time.Millisecond {
			t.Fatalf("attempt 2 backoff out of range [0, 800ms]: got %v", d)
		}
	}

	// Verify large attempt: capped at maxDelay (10s)
	for i := 0; i < 100; i++ {
		d := FullJitterBackoff(20, baseDelay, maxDelay, nil)
		if d < 0 || d > maxDelay {
			t.Fatalf("large attempt backoff exceeds maxDelay %v: got %v", maxDelay, d)
		}
	}
}

func TestFullJitterDeterministicRand(t *testing.T) {
	baseDelay := 200 * time.Millisecond
	maxDelay := 10 * time.Second

	// Mock uniformRand that returns the exact upper bound
	maxRand := func(n int64) int64 {
		return n - 1
	}

	d0 := FullJitterBackoff(0, baseDelay, maxDelay, maxRand)
	if d0 != 200*time.Millisecond {
		t.Fatalf("expected 200ms with max rand, got %v", d0)
	}

	d1 := FullJitterBackoff(1, baseDelay, maxDelay, maxRand)
	if d1 != 400*time.Millisecond {
		t.Fatalf("expected 400ms with max rand, got %v", d1)
	}
}

func TestParseRetryAfterIntegerSeconds(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)

	// Within 5s threshold
	h1 := http.Header{}
	h1.Set("Retry-After", "3")
	dur1, parsed1, exceeds1 := ParseRetryAfter(h1, now)
	if !parsed1 || dur1 != 3*time.Second || exceeds1 {
		t.Fatalf("expected 3s, parsed=true, exceeds=false; got dur=%v, parsed=%v, exceeds=%v", dur1, parsed1, exceeds1)
	}

	// Exceeds 5s threshold
	h2 := http.Header{}
	h2.Set("Retry-After", "12")
	dur2, parsed2, exceeds2 := ParseRetryAfter(h2, now)
	if !parsed2 || dur2 != 12*time.Second || !exceeds2 {
		t.Fatalf("expected 12s, parsed=true, exceeds=true; got dur=%v, parsed=%v, exceeds=%v", dur2, parsed2, exceeds2)
	}
}

func TestParseRetryAfterHTTPDate(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	target := now.Add(10 * time.Second)

	h := http.Header{}
	h.Set("Retry-After", target.Format(http.TimeFormat))

	dur, parsed, exceeds := ParseRetryAfter(h, now)
	if !parsed || dur != 10*time.Second || !exceeds {
		t.Fatalf("expected 10s, parsed=true, exceeds=true; got dur=%v, parsed=%v, exceeds=%v", dur, parsed, exceeds)
	}
}

func TestParseRateLimitResetHeaders(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)

	// Duration format
	h1 := http.Header{}
	h1.Set("x-ratelimit-reset-requests", "250ms")
	dur1, parsed1, exceeds1 := ParseRetryAfter(h1, now)
	if !parsed1 || dur1 != 250*time.Millisecond || exceeds1 {
		t.Fatalf("expected 250ms, parsed=true, exceeds=false; got dur=%v, parsed=%v, exceeds=%v", dur1, parsed1, exceeds1)
	}

	// Relative seconds
	h2 := http.Header{}
	h2.Set("x-ratelimit-reset-tokens", "6")
	dur2, parsed2, exceeds2 := ParseRetryAfter(h2, now)
	if !parsed2 || dur2 != 6*time.Second || !exceeds2 {
		t.Fatalf("expected 6s, parsed=true, exceeds=true; got dur=%v, parsed=%v, exceeds=%v", dur2, parsed2, exceeds2)
	}
}

func TestRetryableStatusCodes(t *testing.T) {
	retryable := []int{502, 503, 504, 429}
	for _, code := range retryable {
		if !IsRetryableStatus(code) {
			t.Fatalf("expected %d to be retryable", code)
		}
	}

	nonRetryable := []int{200, 201, 400, 401, 403, 404, 422}
	for _, code := range nonRetryable {
		if IsRetryableStatus(code) {
			t.Fatalf("expected %d to NOT be retryable", code)
		}
	}

	mustNotRetry := []int{400, 401, 403, 404, 422}
	for _, code := range mustNotRetry {
		if !IsNonRetryableStatus(code) {
			t.Fatalf("expected %d to be non-retryable", code)
		}
	}
}

func TestRetryableNetworkErrors(t *testing.T) {
	if IsRetryableNetworkError(nil) {
		t.Fatal("expected nil error to not be retryable")
	}

	// Context canceled should NOT be retryable
	if IsRetryableNetworkError(context.Canceled) {
		t.Fatal("expected context.Canceled to not be retryable")
	}
	if IsRetryableNetworkError(context.DeadlineExceeded) {
		t.Fatal("expected context.DeadlineExceeded to not be retryable")
	}

	// Connection refused
	if !IsRetryableNetworkError(errors.New("dial tcp 127.0.0.1:8000: connect: connection refused")) {
		t.Fatal("expected connection refused to be retryable")
	}

	// Net error
	opErr := &net.OpError{Op: "dial", Net: "tcp", Err: fmt.Errorf("i/o timeout")}
	if !IsRetryableNetworkError(opErr) {
		t.Fatal("expected net.OpError to be retryable")
	}
}
