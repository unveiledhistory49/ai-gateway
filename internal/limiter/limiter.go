package limiter

import (
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/company/ai-gateway/internal/model"
)

// WindowDuration defines the sliding window size for rate limiting (60 seconds).
const WindowDuration = 60 * time.Second

type tokenUsageEntry struct {
	timestamp time.Time
	count     int
}

type tenantLimiter struct {
	mu               sync.Mutex
	requestTimes     []time.Time
	tokenUsages      []tokenUsageEntry
	reservedTokens   int
	activeConcurrent int
}

// Limiter provides thread-safe, multi-tenant rate limiting and token reservation.
type Limiter struct {
	mu      sync.RWMutex
	tenants map[string]*tenantLimiter
}

// NewLimiter creates a new thread-safe rate limiter.
func NewLimiter() *Limiter {
	return &Limiter{
		tenants: make(map[string]*tenantLimiter),
	}
}

func (l *Limiter) getOrCreateTenant(tenantID string) *tenantLimiter {
	l.mu.RLock()
	tl, ok := l.tenants[tenantID]
	l.mu.RUnlock()
	if ok {
		return tl
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if tl, ok = l.tenants[tenantID]; ok {
		return tl
	}
	tl = &tenantLimiter{}
	l.tenants[tenantID] = tl
	return tl
}

// Reservation holds an active token reservation and concurrency slot.
type Reservation struct {
	tenantID   string
	estTokens  int
	limiter    *Limiter
	reconciled sync.Once
}

// Reconcile settles actual token usage against the speculative reservation using current time.
func (r *Reservation) Reconcile(actualTokens int) {
	r.ReconcileAt(actualTokens, time.Now())
}

// ReconcileAt settles actual token usage at an explicit timestamp.
func (r *Reservation) ReconcileAt(actualTokens int, now time.Time) {
	r.reconciled.Do(func() {
		if r.limiter != nil {
			r.limiter.reconcile(r.tenantID, r.estTokens, actualTokens, now)
		}
	})
}

// Acquire attempts to acquire rate limit permits and speculatively reserve estimated tokens.
func (l *Limiter) Acquire(tenantID string, limits model.RateLimits, estTokens int) (*Reservation, int, error) {
	return l.AcquireAt(tenantID, limits, estTokens, time.Now())
}

// AcquireAt executes acquire logic at an explicit timestamp (useful for deterministic tests).
func (l *Limiter) AcquireAt(tenantID string, limits model.RateLimits, estTokens int, now time.Time) (*Reservation, int, error) {
	tl := l.getOrCreateTenant(tenantID)
	tl.mu.Lock()
	defer tl.mu.Unlock()

	// 1. Concurrency limit check
	if limits.MaxConcurrent > 0 && tl.activeConcurrent >= limits.MaxConcurrent {
		return nil, 1, model.NewGatewayError(
			429,
			model.ErrCodeConcurrencyLimitExceeded,
			fmt.Sprintf("Tenant '%s' exceeded max concurrent requests limit (%d)", tenantID, limits.MaxConcurrent),
		)
	}

	windowStart := now.Add(-WindowDuration)

	// 2. RPM sliding window check
	// Prune timestamps older than 60 seconds
	prunedRequests := tl.requestTimes[:0]
	for _, t := range tl.requestTimes {
		if t.After(windowStart) {
			prunedRequests = append(prunedRequests, t)
		}
	}
	tl.requestTimes = prunedRequests

	if limits.RequestsPerMinute > 0 && len(tl.requestTimes) >= limits.RequestsPerMinute {
		// Calculate retry-after based on oldest entry in window
		retryAfterSec := 1
		if len(tl.requestTimes) > 0 {
			oldest := tl.requestTimes[0]
			retryDur := oldest.Add(WindowDuration).Sub(now)
			retryAfterSec = int(math.Ceil(retryDur.Seconds()))
			if retryAfterSec < 1 {
				retryAfterSec = 1
			}
		}
		return nil, retryAfterSec, model.NewGatewayError(
			429,
			model.ErrCodeRateLimitExceeded,
			fmt.Sprintf("Tenant '%s' exceeded requests per minute limit (%d)", tenantID, limits.RequestsPerMinute),
		)
	}

	// 3. TPM sliding window check with two-phase reservation
	prunedTokens := tl.tokenUsages[:0]
	usedTokens := 0
	for _, entry := range tl.tokenUsages {
		if entry.timestamp.After(windowStart) {
			prunedTokens = append(prunedTokens, entry)
			usedTokens += entry.count
		}
	}
	tl.tokenUsages = prunedTokens

	if limits.TokensPerMinute > 0 && (usedTokens+tl.reservedTokens+estTokens) > limits.TokensPerMinute {
		// Calculate retry-after based on when enough tokens will roll out
		retryAfterSec := 1
		neededRefund := (usedTokens + tl.reservedTokens + estTokens) - limits.TokensPerMinute
		reclaimed := 0
		for _, entry := range tl.tokenUsages {
			reclaimed += entry.count
			if reclaimed >= neededRefund {
				retryDur := entry.timestamp.Add(WindowDuration).Sub(now)
				retryAfterSec = int(math.Ceil(retryDur.Seconds()))
				if retryAfterSec < 1 {
					retryAfterSec = 1
				}
				break
			}
		}
		return nil, retryAfterSec, model.NewGatewayError(
			429,
			model.ErrCodeRateLimitExceeded,
			fmt.Sprintf("Tenant '%s' exceeded tokens per minute limit (%d)", tenantID, limits.TokensPerMinute),
		)
	}

	// All checks passed - increment concurrency, record request time, add reservation
	tl.activeConcurrent++
	tl.requestTimes = append(tl.requestTimes, now)
	tl.reservedTokens += estTokens

	return &Reservation{
		tenantID:  tenantID,
		estTokens: estTokens,
		limiter:   l,
	}, 0, nil
}

// Reconcile handles post-dispatch token settlement and concurrency decrement.
func (l *Limiter) Reconcile(tenantID string, estTokens int, actualTokens int) {
	l.reconcile(tenantID, estTokens, actualTokens, time.Now())
}

// reconcile internal implementation.
func (l *Limiter) reconcile(tenantID string, estTokens int, actualTokens int, now time.Time) {
	tl := l.getOrCreateTenant(tenantID)
	tl.mu.Lock()
	defer tl.mu.Unlock()

	// Decrement concurrency
	if tl.activeConcurrent > 0 {
		tl.activeConcurrent--
	}

	// Deduct reservation
	tl.reservedTokens -= estTokens
	if tl.reservedTokens < 0 {
		tl.reservedTokens = 0
	}

	// Record actual token consumption into sliding window
	if actualTokens > 0 {
		tl.tokenUsages = append(tl.tokenUsages, tokenUsageEntry{
			timestamp: now,
			count:     actualTokens,
		})
	}
}

// ActiveConcurrent returns current in-flight concurrency count for a tenant.
func (l *Limiter) ActiveConcurrent(tenantID string) int {
	tl := l.getOrCreateTenant(tenantID)
	tl.mu.Lock()
	defer tl.mu.Unlock()
	return tl.activeConcurrent
}

// ReservedTokens returns current active reserved tokens for a tenant.
func (l *Limiter) ReservedTokens(tenantID string) int {
	tl := l.getOrCreateTenant(tenantID)
	tl.mu.Lock()
	defer tl.mu.Unlock()
	return tl.reservedTokens
}
