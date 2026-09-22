package resilience

import (
	"context"
	"errors"
	"io"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultBaseDelay  = 200 * time.Millisecond // T_base = 200ms per FAILURE-MODES.md
	DefaultMaxDelay   = 10 * time.Second       // T_max = 10,000ms per FAILURE-MODES.md
	DefaultMaxRetries = 2                      // attempt in {0, 1, 2}
	MaxWaitRetryAfter = 5 * time.Second        // If Retry-After > 5s, abort waiting and failover
)

// RetryConfig defines settings for adaptive retries and backoff.
type RetryConfig struct {
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	MaxRetries  int
	UniformRand func(n int64) int64
}

// ApplyDefaults fills in zero values with defaults.
func (c *RetryConfig) ApplyDefaults() {
	if c.BaseDelay <= 0 {
		c.BaseDelay = DefaultBaseDelay
	}
	if c.MaxDelay <= 0 {
		c.MaxDelay = DefaultMaxDelay
	}
	if c.MaxRetries <= 0 {
		c.MaxRetries = DefaultMaxRetries
	}
	if c.UniformRand == nil {
		c.UniformRand = rand.Int63n
	}
}

// FullJitterBackoff calculates sleep duration using the Full Jitter formula:
// Sleep = Uniform(0, min(T_max, T_base * 2^attempt))
func FullJitterBackoff(attempt int, baseDelay, maxDelay time.Duration, uniformRand func(n int64) int64) time.Duration {
	if baseDelay <= 0 {
		baseDelay = DefaultBaseDelay
	}
	if maxDelay <= 0 {
		maxDelay = DefaultMaxDelay
	}
	if attempt < 0 {
		attempt = 0
	}
	if uniformRand == nil {
		uniformRand = rand.Int63n
	}

	multiplier := int64(1)
	if attempt < 30 {
		multiplier = int64(1) << attempt
	} else {
		multiplier = 1 << 30
	}

	capDelay := float64(baseDelay) * float64(multiplier)
	if capDelay > float64(maxDelay) {
		capDelay = float64(maxDelay)
	}

	if capDelay <= 0 {
		return 0
	}

	maxN := int64(capDelay)
	return time.Duration(uniformRand(maxN + 1))
}

// ParseRetryAfter parses the Retry-After header or rate limit reset headers.
// Returns the duration, whether a valid header was parsed, and whether it exceeds the 5s threshold.
func ParseRetryAfter(header http.Header, now time.Time) (duration time.Duration, parsed bool, exceedsThreshold bool) {
	if header == nil {
		return 0, false, false
	}

	// 1. Check Retry-After
	retryAfter := strings.TrimSpace(header.Get("Retry-After"))
	if retryAfter != "" {
		// Try parsing as integer seconds
		if seconds, err := strconv.Atoi(retryAfter); err == nil && seconds >= 0 {
			d := time.Duration(seconds) * time.Second
			return d, true, d > MaxWaitRetryAfter
		}

		// Try parsing as HTTP-Date format (RFC 1123 / RFC 850 / ANSIC)
		if targetTime, err := http.ParseTime(retryAfter); err == nil {
			d := targetTime.Sub(now)
			if d < 0 {
				d = 0
			}
			return d, true, d > MaxWaitRetryAfter
		}
	}

	// 2. Check x-ratelimit-reset-requests, x-ratelimit-reset-tokens, x-ratelimit-reset
	for _, h := range []string{"x-ratelimit-reset-requests", "x-ratelimit-reset-tokens", "x-ratelimit-reset"} {
		val := strings.TrimSpace(header.Get(h))
		if val == "" {
			continue
		}

		// Try duration format: e.g. "250ms", "2s"
		if d, err := time.ParseDuration(val); err == nil && d >= 0 {
			return d, true, d > MaxWaitRetryAfter
		}

		// Try Unix timestamp or relative seconds
		if num, err := strconv.ParseFloat(val, 64); err == nil && num > 0 {
			if num > 1577836800 { // timestamp after year 2020
				target := time.Unix(int64(num), int64((num-float64(int64(num)))*1e9))
				d := target.Sub(now)
				if d < 0 {
					d = 0
				}
				return d, true, d > MaxWaitRetryAfter
			}
			d := time.Duration(num * float64(time.Second))
			return d, true, d > MaxWaitRetryAfter
		}
	}

	return 0, false, false
}

// IsRetryableStatus returns true for HTTP status codes that represent transient provider failures:
// 502 Bad Gateway, 503 Service Unavailable, 504 Gateway Timeout, 429 Too Many Requests.
func IsRetryableStatus(statusCode int) bool {
	switch statusCode {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout, http.StatusTooManyRequests:
		return true
	default:
		return false
	}
}

// IsNonRetryableStatus returns true for client or policy errors that should never be retried:
// 400 Bad Request, 401 Unauthorized, 403 Forbidden, 404 Not Found, 422 Unprocessable Entity.
func IsNonRetryableStatus(statusCode int) bool {
	switch statusCode {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity:
		return true
	default:
		return false
	}
}

// IsRetryableNetworkError returns true if err is a transient network/connection error.
func IsRetryableNetworkError(err error) bool {
	if err == nil {
		return false
	}

	// Never retry downstream client cancellation
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}

	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}

	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}

	errStr := strings.ToLower(err.Error())
	return strings.Contains(errStr, "connection refused") ||
		strings.Contains(errStr, "connection reset") ||
		strings.Contains(errStr, "broken pipe") ||
		strings.Contains(errStr, "i/o timeout") ||
		strings.Contains(errStr, "no such host")
}
