package resilience

import (
	"log/slog"
	"sync"
	"time"
)

// State represents the current operational state of a circuit breaker.
type State string

const (
	StateClosed   State = "CLOSED"
	StateHalfOpen State = "HALF_OPEN"
	StateOpen     State = "OPEN"
)

// Default Circuit Breaker mathematical parameters per FAILURE-MODES.md.
const (
	DefaultFailureRateThreshold = 0.50             // 50% error rate
	DefaultMinRequests          = 20               // Minimum 20 requests in window to trip
	DefaultWindowDuration       = 60 * time.Second // 60s sliding window
	DefaultNumBuckets           = 60               // 60 buckets (1s each)
	DefaultCooldown             = 30 * time.Second // 30s cooldown before half-open
	DefaultMaxCooldown          = 300 * time.Second // 300s max cooldown
	DefaultSuccessThreshold     = 5                // 5 consecutive probe successes to close
	DefaultMaxCanaryProbes      = 1                // 1 concurrent canary request in half-open
)

// CircuitBreakerConfig defines tuning parameters for a CircuitBreaker instance.
type CircuitBreakerConfig struct {
	FailureRateThreshold float64
	MinRequests          int
	WindowDuration       time.Duration
	NumBuckets           int
	Cooldown             time.Duration
	MaxCooldown          time.Duration
	SuccessThreshold     int
	MaxCanaryProbes      int
	NowFunc              func() time.Time
}

// ApplyDefaults fills in zero-value fields with production defaults.
func (c *CircuitBreakerConfig) ApplyDefaults() {
	if c.FailureRateThreshold <= 0 {
		c.FailureRateThreshold = DefaultFailureRateThreshold
	}
	if c.MinRequests <= 0 {
		c.MinRequests = DefaultMinRequests
	}
	if c.WindowDuration <= 0 {
		c.WindowDuration = DefaultWindowDuration
	}
	if c.NumBuckets <= 0 {
		c.NumBuckets = DefaultNumBuckets
	}
	if c.Cooldown <= 0 {
		c.Cooldown = DefaultCooldown
	}
	if c.MaxCooldown <= 0 {
		c.MaxCooldown = DefaultMaxCooldown
	}
	if c.SuccessThreshold <= 0 {
		c.SuccessThreshold = DefaultSuccessThreshold
	}
	if c.MaxCanaryProbes <= 0 {
		c.MaxCanaryProbes = DefaultMaxCanaryProbes
	}
	if c.NowFunc == nil {
		c.NowFunc = time.Now
	}
}

// bucket stores success and failure counters for a 1-second time slice.
type bucket struct {
	timestamp int64
	successes int64
	failures  int64
}

// CircuitBreaker implements the thread-safe SRE Circuit Breaker state machine:
// Closed, Open, HalfOpen with sliding window error rate evaluation.
type CircuitBreaker struct {
	name   string
	config CircuitBreakerConfig
	logger *slog.Logger

	mu                   sync.Mutex
	state                State
	buckets              []bucket
	currentCooldown      time.Duration
	openUntil            time.Time
	consecutiveSuccesses int
	activeCanaries       int
}

// NewCircuitBreaker initializes a new CircuitBreaker for an upstream target.
func NewCircuitBreaker(name string, cfg CircuitBreakerConfig, logger *slog.Logger) *CircuitBreaker {
	cfg.ApplyDefaults()
	if logger == nil {
		logger = slog.Default()
	}

	return &CircuitBreaker{
		name:            name,
		config:          cfg,
		logger:          logger,
		state:           StateClosed,
		buckets:         make([]bucket, cfg.NumBuckets),
		currentCooldown: cfg.Cooldown,
	}
}

// Name returns the identifier of the upstream target.
func (cb *CircuitBreaker) Name() string {
	return cb.name
}

func (cb *CircuitBreaker) now() time.Time {
	return cb.config.NowFunc()
}

// State returns the current State, evaluating auto-transition from Open to HalfOpen.
func (cb *CircuitBreaker) State() State {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.checkOpenToHalfOpenLocked()
	return cb.state
}

// Allow checks whether an incoming request is permitted to proceed.
// In CLOSED: allows all requests.
// In OPEN: if cooldown elapsed, transitions to HALF_OPEN and allows 1 canary probe; else rejects.
// In HALF_OPEN: allows up to MaxCanaryProbes concurrent requests; else rejects.
func (cb *CircuitBreaker) Allow() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.checkOpenToHalfOpenLocked()

	switch cb.state {
	case StateClosed:
		return true
	case StateHalfOpen:
		if cb.activeCanaries < cb.config.MaxCanaryProbes {
			cb.activeCanaries++
			return true
		}
		return false
	case StateOpen:
		return false
	default:
		return false
	}
}

// RecordSuccess records a successful upstream call.
func (cb *CircuitBreaker) RecordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case StateClosed:
		cb.recordBucketLocked(true)

	case StateHalfOpen:
		if cb.activeCanaries > 0 {
			cb.activeCanaries--
		}
		cb.consecutiveSuccesses++
		if cb.consecutiveSuccesses >= cb.config.SuccessThreshold {
			cb.logger.Info("Circuit breaker state transition",
				"upstream", cb.name,
				"previous_state", string(StateHalfOpen),
				"new_state", string(StateClosed),
				"consecutive_successes", cb.consecutiveSuccesses,
			)
			cb.state = StateClosed
			cb.consecutiveSuccesses = 0
			cb.currentCooldown = cb.config.Cooldown
			cb.clearBucketsLocked()
		}

	case StateOpen:
		// Request succeeded unexpectedly while OPEN; ignore
	}
}

// RecordFailure records a failed upstream call (5xx, 429, timeout, connection error).
func (cb *CircuitBreaker) RecordFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	now := cb.now()

	switch cb.state {
	case StateClosed:
		cb.recordBucketLocked(false)
		totalReqs, failures, rate := cb.calculateErrorRateLocked(now)
		if totalReqs >= int64(cb.config.MinRequests) && rate >= cb.config.FailureRateThreshold {
			cb.openUntil = now.Add(cb.currentCooldown)
			cb.logger.Info("Circuit breaker state transition",
				"upstream", cb.name,
				"previous_state", string(StateClosed),
				"new_state", string(StateOpen),
				"failure_rate", rate,
				"total_requests", totalReqs,
				"failures", failures,
				"cooldown_seconds", cb.currentCooldown.Seconds(),
			)
			cb.state = StateOpen
		}

	case StateHalfOpen:
		if cb.activeCanaries > 0 {
			cb.activeCanaries--
		}
		cb.consecutiveSuccesses = 0

		// Double cooldown on probe failure per FAILURE-MODES.md
		nextCooldown := cb.currentCooldown * 2
		if nextCooldown > cb.config.MaxCooldown {
			nextCooldown = cb.config.MaxCooldown
		}
		cb.currentCooldown = nextCooldown
		cb.openUntil = now.Add(cb.currentCooldown)

		cb.logger.Info("Circuit breaker state transition",
			"upstream", cb.name,
			"previous_state", string(StateHalfOpen),
			"new_state", string(StateOpen),
			"cooldown_seconds", cb.currentCooldown.Seconds(),
		)
		cb.state = StateOpen

	case StateOpen:
		// Already OPEN
	}
}

// Reset restores the circuit breaker to clean CLOSED state.
func (cb *CircuitBreaker) Reset() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.state = StateClosed
	cb.consecutiveSuccesses = 0
	cb.activeCanaries = 0
	cb.currentCooldown = cb.config.Cooldown
	cb.clearBucketsLocked()
}

// checkOpenToHalfOpenLocked checks if cooldown elapsed in OPEN state and transitions to HALF_OPEN.
func (cb *CircuitBreaker) checkOpenToHalfOpenLocked() {
	if cb.state != StateOpen {
		return
	}

	now := cb.now()
	if now.After(cb.openUntil) || now.Equal(cb.openUntil) {
		cb.logger.Info("Circuit breaker state transition",
			"upstream", cb.name,
			"previous_state", string(StateOpen),
			"new_state", string(StateHalfOpen),
			"cooldown_elapsed", cb.currentCooldown.Seconds(),
		)
		cb.state = StateHalfOpen
		cb.consecutiveSuccesses = 0
		cb.activeCanaries = 0
	}
}

// recordBucketLocked increments success or failure count in the active sliding window bucket.
func (cb *CircuitBreaker) recordBucketLocked(success bool) {
	now := cb.now().Unix()
	idx := int(now % int64(cb.config.NumBuckets))
	if idx < 0 {
		idx = -idx
	}

	b := &cb.buckets[idx]
	if b.timestamp != now {
		b.timestamp = now
		b.successes = 0
		b.failures = 0
	}

	if success {
		b.successes++
	} else {
		b.failures++
	}
}

// calculateErrorRateLocked aggregates requests within WindowDuration and computes failure rate.
func (cb *CircuitBreaker) calculateErrorRateLocked(now time.Time) (totalReqs int64, failures int64, rate float64) {
	nowSec := now.Unix()
	windowSec := int64(cb.config.WindowDuration / time.Second)
	if windowSec <= 0 {
		windowSec = 60
	}

	for i := range cb.buckets {
		b := &cb.buckets[i]
		if b.timestamp > 0 && (nowSec-b.timestamp) < windowSec {
			totalReqs += (b.successes + b.failures)
			failures += b.failures
		}
	}

	if totalReqs > 0 {
		rate = float64(failures) / float64(totalReqs)
	}
	return totalReqs, failures, rate
}

// clearBucketsLocked zeroes all window buckets.
func (cb *CircuitBreaker) clearBucketsLocked() {
	for i := range cb.buckets {
		cb.buckets[i] = bucket{}
	}
}

// Registry manages circuit breaker instances per upstream target.
type Registry struct {
	mu        sync.RWMutex
	breakers  map[string]*CircuitBreaker
	defaultCfg CircuitBreakerConfig
	logger    *slog.Logger
}

// NewRegistry initializes a circuit breaker registry.
func NewRegistry(defaultCfg CircuitBreakerConfig, logger *slog.Logger) *Registry {
	defaultCfg.ApplyDefaults()
	if logger == nil {
		logger = slog.Default()
	}

	return &Registry{
		breakers:   make(map[string]*CircuitBreaker),
		defaultCfg: defaultCfg,
		logger:     logger,
	}
}

// Get returns the CircuitBreaker for upstreamID, creating one if it does not yet exist.
func (r *Registry) Get(upstreamID string) *CircuitBreaker {
	r.mu.RLock()
	cb, exists := r.breakers[upstreamID]
	r.mu.RUnlock()
	if exists {
		return cb
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Double check pattern
	if cb, exists = r.breakers[upstreamID]; exists {
		return cb
	}

	cb = NewCircuitBreaker(upstreamID, r.defaultCfg, r.logger)
	r.breakers[upstreamID] = cb
	return cb
}

// GetAll returns a snapshot map of all registered circuit breakers.
func (r *Registry) GetAll() map[string]*CircuitBreaker {
	r.mu.RLock()
	defer r.mu.RUnlock()

	copyMap := make(map[string]*CircuitBreaker, len(r.breakers))
	for k, v := range r.breakers {
		copyMap[k] = v
	}
	return copyMap
}

// Register explicitly stores a pre-configured CircuitBreaker.
func (r *Registry) Register(upstreamID string, cb *CircuitBreaker) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.breakers[upstreamID] = cb
}
