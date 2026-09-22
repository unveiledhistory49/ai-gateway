package resilience

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestHealthCheckerHealthyUpstream(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	}))
	defer mockServer.Close()

	upstreams := []UpstreamEndpoint{
		{ID: "up-1", EndpointURL: mockServer.URL},
	}
	reg := NewRegistry(CircuitBreakerConfig{}, newTestLogger())
	hc := NewHealthChecker(upstreams, reg, HealthCheckerConfig{
		Interval: 50 * time.Millisecond,
		Timeout:  100 * time.Millisecond,
	}, newTestLogger())

	hc.Start()
	defer hc.Stop()

	time.Sleep(20 * time.Millisecond)

	if !hc.IsUpstreamHealthy("up-1") {
		t.Fatal("expected up-1 to be healthy")
	}
	if !hc.IsReady() {
		t.Fatal("expected gateway readiness to be true")
	}
}

func TestHealthCheckerUnhealthyUpstream(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer mockServer.Close()

	upstreams := []UpstreamEndpoint{
		{ID: "up-bad", EndpointURL: mockServer.URL},
	}
	reg := NewRegistry(CircuitBreakerConfig{}, newTestLogger())
	hc := NewHealthChecker(upstreams, reg, HealthCheckerConfig{
		Interval: 50 * time.Millisecond,
		Timeout:  100 * time.Millisecond,
	}, newTestLogger())

	hc.Start()
	defer hc.Stop()

	time.Sleep(20 * time.Millisecond)

	if hc.IsUpstreamHealthy("up-bad") {
		t.Fatal("expected up-bad to be unhealthy")
	}
	if hc.IsReady() {
		t.Fatal("expected gateway readiness to be false when all upstreams are down")
	}
}

func TestHealthCheckerReadinessWithCircuitBreakerOpen(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer mockServer.Close()

	upstreams := []UpstreamEndpoint{
		{ID: "up-tripped", EndpointURL: mockServer.URL},
	}
	reg := NewRegistry(CircuitBreakerConfig{
		MinRequests: 1,
	}, newTestLogger())

	hc := NewHealthChecker(upstreams, reg, HealthCheckerConfig{
		Interval: 50 * time.Millisecond,
		Timeout:  100 * time.Millisecond,
	}, newTestLogger())

	hc.Start()
	defer hc.Stop()

	time.Sleep(20 * time.Millisecond)
	if !hc.IsReady() {
		t.Fatal("expected initially ready")
	}

	// Trip the circuit breaker for up-tripped
	cb := reg.Get("up-tripped")
	cb.RecordFailure()

	if cb.State() != StateOpen {
		t.Fatalf("expected breaker to be OPEN, got %s", cb.State())
	}

	// Now IsReady() must report false because its circuit breaker is OPEN
	if hc.IsReady() {
		t.Fatal("expected readiness to be false when circuit breaker is OPEN")
	}
}

func TestHealthCheckerProbeCounts(t *testing.T) {
	var probeCount int32
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&probeCount, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer mockServer.Close()

	upstreams := []UpstreamEndpoint{
		{ID: "up-1", EndpointURL: mockServer.URL},
	}
	hc := NewHealthChecker(upstreams, nil, HealthCheckerConfig{
		Interval: 30 * time.Millisecond,
		Timeout:  50 * time.Millisecond,
	}, newTestLogger())

	hc.Start()
	time.Sleep(80 * time.Millisecond)
	hc.Stop()

	count := atomic.LoadInt32(&probeCount)
	if count < 2 {
		t.Fatalf("expected at least 2 probes, got %d", count)
	}
}
