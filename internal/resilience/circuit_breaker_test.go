package resilience

import (
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestCircuitBreakerInitialClosed(t *testing.T) {
	cb := NewCircuitBreaker("test-target", CircuitBreakerConfig{}, newTestLogger())
	if state := cb.State(); state != StateClosed {
		t.Fatalf("expected state %s, got %s", StateClosed, state)
	}
	if !cb.Allow() {
		t.Fatal("expected Allow() to return true in Closed state")
	}
}

func TestCircuitBreakerMinRequestsNotTripped(t *testing.T) {
	fakeTime := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	cfg := CircuitBreakerConfig{
		FailureRateThreshold: 0.50,
		MinRequests:          20,
		NowFunc:              func() time.Time { return fakeTime },
	}
	cb := NewCircuitBreaker("test-target", cfg, newTestLogger())

	// Send 19 failures (less than MinRequests=20)
	for i := 0; i < 19; i++ {
		cb.RecordFailure()
	}

	if state := cb.State(); state != StateClosed {
		t.Fatalf("expected breaker to remain Closed when requests < MinRequests, got %s", state)
	}
	if !cb.Allow() {
		t.Fatal("expected Allow() to be true")
	}
}

func TestCircuitBreakerErrorRateThreshold(t *testing.T) {
	fakeTime := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	cfg := CircuitBreakerConfig{
		FailureRateThreshold: 0.50,
		MinRequests:          20,
		Cooldown:             30 * time.Second,
		NowFunc:              func() time.Time { return fakeTime },
	}
	cb := NewCircuitBreaker("test-target", cfg, newTestLogger())

	// 10 successes, 9 failures (9/19 = 47.3% < 50%)
	for i := 0; i < 10; i++ {
		cb.RecordSuccess()
	}
	for i := 0; i < 9; i++ {
		cb.RecordFailure()
	}
	if cb.State() != StateClosed {
		t.Fatalf("expected Closed state at 47.3%% error rate, got %s", cb.State())
	}

	// 1 more failure (10 failures / 20 total = 50.0% == threshold) -> trips to OPEN!
	cb.RecordFailure()
	if cb.State() != StateOpen {
		t.Fatalf("expected Open state at 50%% error rate with 20 requests, got %s", cb.State())
	}
	if cb.Allow() {
		t.Fatal("expected Allow() to be false in Open state")
	}
}

func TestCircuitBreakerFullLifecycleClosedToOpenToHalfOpenToClosed(t *testing.T) {
	currentTime := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	timeLock := sync.Mutex{}
	nowFunc := func() time.Time {
		timeLock.Lock()
		defer timeLock.Unlock()
		return currentTime
	}

	cfg := CircuitBreakerConfig{
		FailureRateThreshold: 0.50,
		MinRequests:          20,
		Cooldown:             30 * time.Second,
		SuccessThreshold:     5,
		MaxCanaryProbes:      1,
		NowFunc:              nowFunc,
	}
	cb := NewCircuitBreaker("test-target", cfg, newTestLogger())

	// 1. Trip breaker to OPEN: 20 failures
	for i := 0; i < 20; i++ {
		cb.RecordFailure()
	}
	if cb.State() != StateOpen {
		t.Fatalf("expected OPEN state, got %s", cb.State())
	}

	// In OPEN: requests rejected
	if cb.Allow() {
		t.Fatal("expected Allow() to return false in OPEN")
	}

	// Advance time by 29 seconds (cooldown is 30s) -> still OPEN
	timeLock.Lock()
	currentTime = currentTime.Add(29 * time.Second)
	timeLock.Unlock()

	if cb.State() != StateOpen {
		t.Fatalf("expected still OPEN at 29s, got %s", cb.State())
	}
	if cb.Allow() {
		t.Fatal("expected Allow() to return false at 29s")
	}

	// Advance time to 30s -> transitions to HALF_OPEN on Allow()
	timeLock.Lock()
	currentTime = currentTime.Add(1 * time.Second)
	timeLock.Unlock()

	// 1st canary probe allowed
	if !cb.Allow() {
		t.Fatal("expected Allow() to return true for 1st canary probe after cooldown")
	}
	if cb.State() != StateHalfOpen {
		t.Fatalf("expected HALF_OPEN state, got %s", cb.State())
	}

	// 2nd concurrent probe should be rejected because MaxCanaryProbes = 1
	if cb.Allow() {
		t.Fatal("expected concurrent probe to be rejected while 1 probe is active")
	}

	// 1st probe succeeds
	cb.RecordSuccess()

	// Now send 4 more successes (total 5 consecutive successes)
	for i := 0; i < 4; i++ {
		if !cb.Allow() {
			t.Fatalf("expected probe %d to be allowed", i+2)
		}
		cb.RecordSuccess()
	}

	// After 5 consecutive successes -> transitions back to CLOSED!
	if cb.State() != StateClosed {
		t.Fatalf("expected CLOSED state after 5 consecutive successes, got %s", cb.State())
	}
	if !cb.Allow() {
		t.Fatal("expected Allow() to be true in CLOSED state")
	}
}

func TestCircuitBreakerHalfOpenFailureReTripsWithDoubledCooldown(t *testing.T) {
	currentTime := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	timeLock := sync.Mutex{}
	nowFunc := func() time.Time {
		timeLock.Lock()
		defer timeLock.Unlock()
		return currentTime
	}

	cfg := CircuitBreakerConfig{
		FailureRateThreshold: 0.50,
		MinRequests:          20,
		Cooldown:             30 * time.Second,
		MaxCooldown:          300 * time.Second,
		SuccessThreshold:     5,
		NowFunc:              nowFunc,
	}
	cb := NewCircuitBreaker("test-target", cfg, newTestLogger())

	// Trip to OPEN
	for i := 0; i < 20; i++ {
		cb.RecordFailure()
	}
	if cb.State() != StateOpen {
		t.Fatalf("expected OPEN, got %s", cb.State())
	}

	// Advance time 30s to enter HALF_OPEN
	timeLock.Lock()
	currentTime = currentTime.Add(30 * time.Second)
	timeLock.Unlock()

	if !cb.Allow() {
		t.Fatal("expected Allow() to return true")
	}
	if cb.State() != StateHalfOpen {
		t.Fatalf("expected HALF_OPEN, got %s", cb.State())
	}

	// Probe fails -> must immediately return to OPEN with doubled cooldown (60s)
	cb.RecordFailure()

	if cb.State() != StateOpen {
		t.Fatalf("expected re-trip to OPEN on probe failure, got %s", cb.State())
	}

	// Advance by 30s -> should still be OPEN (new cooldown is 60s)
	timeLock.Lock()
	currentTime = currentTime.Add(30 * time.Second)
	timeLock.Unlock()

	if cb.State() != StateOpen {
		t.Fatalf("expected OPEN at 30s after re-trip with 60s cooldown, got %s", cb.State())
	}
	if cb.Allow() {
		t.Fatal("expected Allow() to be false during doubled cooldown")
	}

	// Advance another 30s (total 60s) -> now can transition to HALF_OPEN
	timeLock.Lock()
	currentTime = currentTime.Add(30 * time.Second)
	timeLock.Unlock()

	if !cb.Allow() {
		t.Fatal("expected Allow() to be true at 60s")
	}
	if cb.State() != StateHalfOpen {
		t.Fatalf("expected HALF_OPEN after doubled cooldown, got %s", cb.State())
	}
}

func TestRegistry(t *testing.T) {
	reg := NewRegistry(CircuitBreakerConfig{}, newTestLogger())

	cb1 := reg.Get("target-1")
	if cb1 == nil || cb1.Name() != "target-1" {
		t.Fatalf("unexpected breaker for target-1: %v", cb1)
	}

	// Same target returns same instance
	cb1Again := reg.Get("target-1")
	if cb1 != cb1Again {
		t.Fatal("expected Get to return same instance for target-1")
	}

	cb2 := reg.Get("target-2")
	if cb2 == nil || cb2.Name() != "target-2" {
		t.Fatalf("unexpected breaker for target-2: %v", cb2)
	}

	all := reg.GetAll()
	if len(all) != 2 {
		t.Fatalf("expected 2 breakers in registry, got %d", len(all))
	}
}
