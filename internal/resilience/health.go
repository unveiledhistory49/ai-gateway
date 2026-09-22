package resilience

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// UpstreamEndpoint holds identification and connection info needed for health checks.
type UpstreamEndpoint struct {
	ID          string
	EndpointURL string
}

// HealthCheckerConfig defines settings for the background health checking daemon.
type HealthCheckerConfig struct {
	Interval   time.Duration
	Timeout    time.Duration
	HTTPClient *http.Client
}

// ApplyDefaults applies default settings.
func (c *HealthCheckerConfig) ApplyDefaults() {
	if c.Interval <= 0 {
		c.Interval = 10 * time.Second
	}
	if c.Timeout <= 0 {
		c.Timeout = 2 * time.Second
	}
	if c.HTTPClient == nil {
		c.HTTPClient = &http.Client{
			Timeout: c.Timeout,
		}
	}
}

// HealthChecker periodically probes configured upstreams and monitors readiness.
type HealthChecker struct {
	upstreams   []UpstreamEndpoint
	registry    *Registry
	cfg         HealthCheckerConfig
	logger      *slog.Logger

	mu          sync.RWMutex
	healthyMap  map[string]bool
	stopChan    chan struct{}
	stoppedChan chan struct{}
	started     bool
}

// NewHealthChecker initializes a new HealthChecker instance.
func NewHealthChecker(upstreams []UpstreamEndpoint, registry *Registry, cfg HealthCheckerConfig, logger *slog.Logger) *HealthChecker {
	cfg.ApplyDefaults()
	if logger == nil {
		logger = slog.Default()
	}

	healthyMap := make(map[string]bool, len(upstreams))
	for _, u := range upstreams {
		healthyMap[u.ID] = true // Optimistically assume healthy until first check completes
	}

	return &HealthChecker{
		upstreams:   upstreams,
		registry:    registry,
		cfg:         cfg,
		logger:      logger,
		healthyMap:  healthyMap,
		stopChan:    make(chan struct{}),
		stoppedChan: make(chan struct{}),
	}
}

// Start launches the background health-checking loop.
func (h *HealthChecker) Start() {
	h.mu.Lock()
	if h.started {
		h.mu.Unlock()
		return
	}
	h.started = true
	h.mu.Unlock()

	// Perform initial synchronous probe
	h.checkAll()

	go h.runLoop()
}

// Stop gracefully terminates the background health-checking daemon.
func (h *HealthChecker) Stop() {
	h.mu.Lock()
	if !h.started {
		h.mu.Unlock()
		return
	}
	h.started = false
	close(h.stopChan)
	h.mu.Unlock()

	<-h.stoppedChan
}

// runLoop executes the periodic probe ticker.
func (h *HealthChecker) runLoop() {
	defer close(h.stoppedChan)

	ticker := time.NewTicker(h.cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-h.stopChan:
			return
		case <-ticker.C:
			h.checkAll()
		}
	}
}

// checkAll executes health checks against all registered upstreams concurrently.
func (h *HealthChecker) checkAll() {
	var wg sync.WaitGroup
	for _, u := range h.upstreams {
		wg.Add(1)
		go func(endpoint UpstreamEndpoint) {
			defer wg.Done()
			h.checkUpstream(endpoint)
		}(u)
	}
	wg.Wait()
}

// checkUpstream performs an active health probe against a single upstream.
func (h *HealthChecker) checkUpstream(endpoint UpstreamEndpoint) {
	ctx, cancel := context.WithTimeout(context.Background(), h.cfg.Timeout)
	defer cancel()

	healthy := h.probeEndpoint(ctx, endpoint.EndpointURL)

	h.mu.Lock()
	prevHealthy := h.healthyMap[endpoint.ID]
	h.healthyMap[endpoint.ID] = healthy
	h.mu.Unlock()

	if prevHealthy != healthy {
		h.logger.Info("Upstream health status changed",
			"upstream_id", endpoint.ID,
			"healthy", healthy,
		)
	}

	// Integrate with Circuit Breaker registry:
	// If upstream is healthy and circuit breaker is in HALF_OPEN, probe can contribute to recovery
	if h.registry != nil && healthy {
		cb := h.registry.Get(endpoint.ID)
		if cb.State() == StateHalfOpen {
			cb.RecordSuccess()
		}
	}
}

// probeEndpoint tests connectivity to the target URL.
// Returns true if server responds with HTTP status < 500.
func (h *HealthChecker) probeEndpoint(ctx context.Context, rawURL string) bool {
	probeURL := strings.TrimRight(rawURL, "/") + "/healthz"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
	if err != nil {
		return false
	}

	resp, err := h.cfg.HTTPClient.Do(req)
	if err != nil {
		// Fallback probe to root URL
		rootURL := strings.TrimRight(rawURL, "/")
		rootReq, rootErr := http.NewRequestWithContext(ctx, http.MethodGet, rootURL, nil)
		if rootErr != nil {
			return false
		}
		rootResp, rootErr := h.cfg.HTTPClient.Do(rootReq)
		if rootErr != nil {
			return false
		}
		defer rootResp.Body.Close()
		return rootResp.StatusCode < 500
	}
	defer resp.Body.Close()

	// If /healthz returns 404, probe root URL
	if resp.StatusCode == http.StatusNotFound {
		rootURL := strings.TrimRight(rawURL, "/")
		rootReq, rootErr := http.NewRequestWithContext(ctx, http.MethodGet, rootURL, nil)
		if rootErr == nil {
			if rootResp, rErr := h.cfg.HTTPClient.Do(rootReq); rErr == nil {
				defer rootResp.Body.Close()
				return rootResp.StatusCode < 500
			}
		}
	}

	return resp.StatusCode < 500
}

// IsUpstreamHealthy returns whether a specific upstream passed its active health probe.
func (h *HealthChecker) IsUpstreamHealthy(upstreamID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.healthyMap[upstreamID]
}

// IsReady evaluates overall gateway readiness for /healthz/readiness:
// Returns true if at least one configured upstream is healthy AND its circuit breaker is not OPEN.
// If all upstreams are failing health checks or have OPEN circuit breakers, returns false.
func (h *HealthChecker) IsReady() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if len(h.upstreams) == 0 {
		return false
	}

	for _, u := range h.upstreams {
		isHealthy := h.healthyMap[u.ID]
		isBreakerOpen := false
		if h.registry != nil {
			cb := h.registry.Get(u.ID)
			isBreakerOpen = (cb.State() == StateOpen)
		}

		if isHealthy && !isBreakerOpen {
			return true
		}
	}

	return false
}

// Snapshot returns a copy of current upstream health statuses.
func (h *HealthChecker) Snapshot() map[string]bool {
	h.mu.RLock()
	defer h.mu.RUnlock()

	res := make(map[string]bool, len(h.healthyMap))
	for k, v := range h.healthyMap {
		res[k] = v
	}
	return res
}
