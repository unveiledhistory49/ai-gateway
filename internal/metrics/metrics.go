package metrics

import (
	"net/http"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Standard bucket configurations according to docs/SLO.md and Layer 4 specification.
var (
	// RequestDurationBuckets spans from 50ms to 120s covering LLM completions.
	RequestDurationBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0, 10.0, 30.0, 60.0, 120.0}

	// OverheadDurationBuckets provides fine-grained millisecond resolution (.0005 to .100)
	// isolating gateway proxy overhead from upstream model inference duration.
	OverheadDurationBuckets = []float64{.0005, .001, .002, .005, .008, .010, .015, .025, .050, .100}

	// UpstreamDurationBuckets measures upstream LLM network and inference latency.
	UpstreamDurationBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0, 10.0, 20.0, 45.0, 90.0}
)

// Metrics holds the Prometheus metric instruments for the AI Gateway.
type Metrics struct {
	registerer prometheus.Registerer
	gatherer   prometheus.Gatherer

	RequestsTotal            *prometheus.CounterVec
	RequestDurationSeconds   *prometheus.HistogramVec
	OverheadDurationSeconds  *prometheus.HistogramVec
	UpstreamDurationSeconds  *prometheus.HistogramVec
	TokensTotal              *prometheus.CounterVec
	CircuitBreakerState      *prometheus.GaugeVec
	RateLimitRejectionsTotal *prometheus.CounterVec
	PolicyViolationsTotal    *prometheus.CounterVec
}

var (
	defaultMetrics *Metrics
	defaultOnce    sync.Once
)

// Default returns the process-wide default Metrics instance.
func Default() *Metrics {
	defaultOnce.Do(func() {
		reg := prometheus.NewRegistry()
		defaultMetrics = NewMetrics(reg)
	})
	return defaultMetrics
}

// NewMetrics initializes and registers all Prometheus instruments.
// If reg is nil, a new isolated prometheus.Registry is created.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	var gatherer prometheus.Gatherer
	if reg == nil {
		r := prometheus.NewRegistry()
		reg = r
		gatherer = r
	} else if g, ok := reg.(prometheus.Gatherer); ok {
		gatherer = g
	} else {
		gatherer = prometheus.DefaultGatherer
	}

	m := &Metrics{
		registerer: reg,
		gatherer:   gatherer,

		RequestsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "ai_gateway_requests_total",
				Help: "Total count of all requests processed at the gateway ingress.",
			},
			[]string{"tenant", "route", "status", "model"},
		),

		RequestDurationSeconds: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "ai_gateway_request_duration_seconds",
				Help:    "End-to-end client request duration including upstream inference and streaming.",
				Buckets: RequestDurationBuckets,
			},
			[]string{"tenant", "route"},
		),

		OverheadDurationSeconds: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "ai_gateway_overhead_duration_seconds",
				Help:    "Time spent exclusively executing gateway proxy logic isolating gateway overhead from upstream inference.",
				Buckets: OverheadDurationBuckets,
			},
			[]string{"tenant", "route"},
		),

		UpstreamDurationSeconds: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "ai_gateway_upstream_duration_seconds",
				Help:    "Duration of network interaction with upstream LLM providers.",
				Buckets: UpstreamDurationBuckets,
			},
			[]string{"upstream", "model", "status"},
		),

		TokensTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "ai_gateway_tokens_total",
				Help: "Total count of LLM tokens processed through the gateway.",
			},
			[]string{"tenant", "model", "type"},
		),

		CircuitBreakerState: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "ai_gateway_circuit_breaker_state",
				Help: "Current state of the circuit breaker for each upstream target (0=Closed, 1=HalfOpen, 2=Open).",
			},
			[]string{"upstream"},
		),

		RateLimitRejectionsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "ai_gateway_ratelimit_rejections_total",
				Help: "Total count of requests rejected due to quota or rate-limit enforcement.",
			},
			[]string{"tenant", "type"},
		),

		PolicyViolationsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "ai_gateway_policy_violations_total",
				Help: "Total count of policy violations detected by deterministic DLP engines.",
			},
			[]string{"tenant", "policy", "action"},
		),
	}

	reg.MustRegister(
		m.RequestsTotal,
		m.RequestDurationSeconds,
		m.OverheadDurationSeconds,
		m.UpstreamDurationSeconds,
		m.TokensTotal,
		m.CircuitBreakerState,
		m.RateLimitRejectionsTotal,
		m.PolicyViolationsTotal,
	)

	return m
}

// Handler returns an http.Handler that serves metrics in Prometheus exposition format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.gatherer, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
	})
}

// RecordRequest observes request execution, incrementing total requests and recording durations.
func (m *Metrics) RecordRequest(tenant, route, status, model string, durationSec, overheadSec float64) {
	if tenant == "" {
		tenant = "unknown"
	}
	if route == "" {
		route = "unknown"
	}
	if status == "" {
		status = "unknown"
	}
	if model == "" {
		model = "unknown"
	}

	m.RequestsTotal.WithLabelValues(tenant, route, status, model).Inc()
	m.RequestDurationSeconds.WithLabelValues(tenant, route).Observe(durationSec)
	if overheadSec >= 0 {
		m.OverheadDurationSeconds.WithLabelValues(tenant, route).Observe(overheadSec)
	}
}

// RecordUpstream observes network and inference duration for an upstream target.
func (m *Metrics) RecordUpstream(upstream, model, status string, durationSec float64) {
	if upstream == "" {
		upstream = "unknown"
	}
	if model == "" {
		model = "unknown"
	}
	if status == "" {
		status = "unknown"
	}

	m.UpstreamDurationSeconds.WithLabelValues(upstream, model, status).Observe(durationSec)
}

// RecordTokens records prompt and completion tokens.
func (m *Metrics) RecordTokens(tenant, model string, promptTokens, completionTokens int) {
	if tenant == "" {
		tenant = "unknown"
	}
	if model == "" {
		model = "unknown"
	}

	if promptTokens > 0 {
		m.TokensTotal.WithLabelValues(tenant, model, "prompt").Add(float64(promptTokens))
	}
	if completionTokens > 0 {
		m.TokensTotal.WithLabelValues(tenant, model, "completion").Add(float64(completionTokens))
	}
}

// RecordRateLimitRejection increments rejection counter for a tenant and limit type (rpm, tpm, concurrency).
func (m *Metrics) RecordRateLimitRejection(tenant, limitType string) {
	if tenant == "" {
		tenant = "unknown"
	}
	if limitType == "" {
		limitType = "rpm"
	}

	m.RateLimitRejectionsTotal.WithLabelValues(tenant, limitType).Inc()
}

// RecordPolicyViolation increments policy violation counter for a tenant, rule name, and enforcement action.
func (m *Metrics) RecordPolicyViolation(tenant, policy, action string) {
	if tenant == "" {
		tenant = "unknown"
	}
	if policy == "" {
		policy = "unknown"
	}
	if action == "" {
		action = "BLOCK"
	}

	m.PolicyViolationsTotal.WithLabelValues(tenant, policy, action).Inc()
}

// SetCircuitBreakerState sets the circuit breaker gauge value for an upstream (0=Closed, 1=HalfOpen, 2=Open).
func (m *Metrics) SetCircuitBreakerState(upstream string, state int) {
	if upstream == "" {
		upstream = "unknown"
	}
	m.CircuitBreakerState.WithLabelValues(upstream).Set(float64(state))
}
