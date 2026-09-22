package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/company/ai-gateway/internal/audit"
	"github.com/company/ai-gateway/internal/auth"
	"github.com/company/ai-gateway/internal/config"
	"github.com/company/ai-gateway/internal/dispatcher"
	"github.com/company/ai-gateway/internal/limiter"
	"github.com/company/ai-gateway/internal/metrics"
	"github.com/company/ai-gateway/internal/model"
	"github.com/company/ai-gateway/internal/pipeline"
	"github.com/company/ai-gateway/internal/policy"
	"github.com/company/ai-gateway/internal/resilience"
	"github.com/company/ai-gateway/internal/router"
	"github.com/company/ai-gateway/internal/tracing"
)

// Server encapsulates the HTTP ingress server, routing table, and processing pipeline.
type Server struct {
	cfg           *config.Config
	pipeline      *pipeline.Pipeline
	authStage     *auth.Stage
	registry      *resilience.Registry
	healthChecker *resilience.HealthChecker
	limiter       *limiter.Limiter
	policyEngine  *policy.Engine
	metrics       *metrics.Metrics
	auditLedger   *audit.Ledger
	httpServer    *http.Server
	logger        *slog.Logger
}

// NewServer initializes a new Server using standard production stages.
func NewServer(cfg *config.Config, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}

	metricsEngine := metrics.Default()

	registry := resilience.NewRegistry(resilience.CircuitBreakerConfig{}, logger)
	registry.SetStateChangeCallback(func(upstream string, from, to resilience.State) {
		var stateVal int
		switch to {
		case resilience.StateClosed:
			stateVal = 0
		case resilience.StateHalfOpen:
			stateVal = 1
		case resilience.StateOpen:
			stateVal = 2
		}
		metricsEngine.SetCircuitBreakerState(upstream, stateVal)
	})

	endpoints := make([]resilience.UpstreamEndpoint, len(cfg.Upstreams))
	for i, u := range cfg.Upstreams {
		endpoints[i] = resilience.UpstreamEndpoint{
			ID:          u.ID,
			EndpointURL: u.EndpointURL,
		}
		metricsEngine.SetCircuitBreakerState(u.ID, 0)
	}

	healthChecker := resilience.NewHealthChecker(endpoints, registry, resilience.HealthCheckerConfig{}, logger)

	limiterInstance := limiter.NewLimiter()
	policyEngine := policy.NewEngine()

	authStage := auth.NewStage(cfg.Tenants, cfg.Server.MaxBodyBytes)
	quotaStage := limiter.NewQuotaStage(limiterInstance, metricsEngine)
	routeStage := router.NewStage(cfg, registry)
	prePolicyStage := policy.NewPrePolicyStage(policyEngine, cfg)
	prePolicyStage.SetMetrics(metricsEngine)
	dispStage := dispatcher.NewStage(30*time.Second, registry)
	dispStage.SetMetrics(metricsEngine)
	postPolicyStage := policy.NewPostPolicyStage(policyEngine)
	postPolicyStage.SetMetrics(metricsEngine)

	pipe := pipeline.NewPipeline(authStage, quotaStage, routeStage, prePolicyStage, dispStage, postPolicyStage)

	s := NewServerWithPipeline(cfg, pipe, authStage, logger)
	s.registry = registry
	s.healthChecker = healthChecker
	s.limiter = limiterInstance
	s.policyEngine = policyEngine
	s.metrics = metricsEngine

	// Initialize file-backed audit ledger; fallback to in-memory on error
	auditFile, err := os.OpenFile("audit.jsonl", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err == nil {
		s.auditLedger = audit.NewLedger(auditFile)
	} else {
		s.auditLedger = audit.NewLedger(nil)
	}
	return s
}

// NewServerWithPipeline constructs a Server with a custom pipeline (useful for testing).
func NewServerWithPipeline(cfg *config.Config, pipe *pipeline.Pipeline, authStage *auth.Stage, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}

	metricsEngine := metrics.Default()
	auditLedger := audit.NewLedger(nil)

	s := &Server{
		cfg:         cfg,
		pipeline:    pipe,
		authStage:   authStage,
		metrics:     metricsEngine,
		auditLedger: auditLedger,
		logger:      logger,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("GET /v1/models", s.handleListModels)
	mux.HandleFunc("GET /healthz/liveness", s.handleLiveness)
	mux.HandleFunc("GET /healthz/readiness", s.handleReadiness)
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		if s.metrics != nil {
			s.metrics.Handler().ServeHTTP(w, r)
		}
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		if s.metrics != nil {
			s.metrics.Handler().ServeHTTP(w, r)
		}
	})

	s.httpServer = &http.Server{
		Addr:         fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port),
		Handler:      s.loggingMiddleware(mux),
		ReadTimeout:  cfg.Server.ReadTimeout(),
		WriteTimeout: cfg.Server.WriteTimeout(),
		IdleTimeout:  cfg.Server.IdleTimeout(),
	}

	return s
}

// Registry returns the circuit breaker registry.
func (s *Server) Registry() *resilience.Registry {
	return s.registry
}

// HealthChecker returns the background health checker daemon.
func (s *Server) HealthChecker() *resilience.HealthChecker {
	return s.healthChecker
}

// SetHealthChecker sets a custom health checker on the server.
func (s *Server) SetHealthChecker(hc *resilience.HealthChecker) {
	s.healthChecker = hc
}

// SetRegistry sets a custom circuit breaker registry on the server.
func (s *Server) SetRegistry(reg *resilience.Registry) {
	s.registry = reg
}

// Start runs the HTTP server listener and background daemons.
func (s *Server) Start() error {
	s.logger.Info("Starting AI Gateway HTTP Ingress Server",
		"host", s.cfg.Server.Host,
		"port", s.cfg.Server.Port,
	)

	if s.healthChecker != nil {
		s.healthChecker.Start()
	}

	if err := s.httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("HTTP ingress listener error: %w", err)
	}
	return nil
}

// Shutdown gracefully terminates in-flight connections within the provided context deadline.
func (s *Server) Shutdown(ctx context.Context) error {
	s.logger.Info("Draining in-flight connections and shutting down HTTP Ingress Server...")

	if s.healthChecker != nil {
		s.healthChecker.Stop()
	}

	return s.httpServer.Shutdown(ctx)
}

// Limiter returns the in-memory rate limiter instance.
func (s *Server) Limiter() *limiter.Limiter {
	return s.limiter
}

// PolicyEngine returns the policy engine instance.
func (s *Server) PolicyEngine() *policy.Engine {
	return s.policyEngine
}

// Metrics returns the Prometheus metrics engine.
func (s *Server) Metrics() *metrics.Metrics {
	return s.metrics
}

// SetMetrics assigns a custom metrics engine.
func (s *Server) SetMetrics(m *metrics.Metrics) {
	s.metrics = m
	if s.registry != nil {
		s.registry.SetStateChangeCallback(func(upstream string, from, to resilience.State) {
			var stateVal int
			switch to {
			case resilience.StateClosed:
				stateVal = 0
			case resilience.StateHalfOpen:
				stateVal = 1
			case resilience.StateOpen:
				stateVal = 2
			}
			m.SetCircuitBreakerState(upstream, stateVal)
		})
	}
	if s.pipeline != nil {
		for _, stage := range s.pipeline.Stages() {
			if qs, ok := stage.(*limiter.QuotaStage); ok {
				qs.SetMetrics(m)
			}
			if ps, ok := stage.(*policy.PrePolicyStage); ok {
				ps.SetMetrics(m)
			}
			if ds, ok := stage.(*dispatcher.Stage); ok {
				ds.SetMetrics(m)
			}
			if po, ok := stage.(*policy.PostPolicyStage); ok {
				po.SetMetrics(m)
			}
		}
	}
}

// AuditLedger returns the cryptographically chained audit ledger.
func (s *Server) AuditLedger() *audit.Ledger {
	return s.auditLedger
}

// SetAuditLedger assigns a custom audit ledger.
func (s *Server) SetAuditLedger(al *audit.Ledger) {
	s.auditLedger = al
}

// Handler exposes the internal http.Handler for testing purposes.
func (s *Server) Handler() http.Handler {
	return s.httpServer.Handler
}

// handleChatCompletions processes inbound OpenAI-compatible chat completion requests.
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	// 1. W3C Trace Context & Request ID extraction
	traceID, spanID := tracing.ExtractOrGenerate(r.Header.Get(tracing.TraceparentHeader))
	reqID := r.Header.Get(tracing.RequestIDHeader)
	if reqID == "" {
		reqID = tracing.NewRequestID()
	}

	// Downstream response headers
	w.Header().Set(tracing.TraceparentHeader, tracing.FormatTraceparent(traceID, spanID))
	w.Header().Set(tracing.RequestIDHeader, reqID)

	reqCtx := pipeline.NewRequestContext(r.Context(), w, r)
	reqCtx.TraceID = traceID
	reqCtx.SpanID = spanID
	reqCtx.RequestID = reqID

	var execErr error
	defer func() {
		reqCtx.ExecuteReconcile(0)

		statusCode := http.StatusOK
		if execErr != nil {
			var gwErr *model.GatewayError
			if errors.As(execErr, &gwErr) {
				statusCode = gwErr.StatusCode
			} else {
				statusCode = http.StatusInternalServerError
			}
		}

		totalDuration := time.Since(reqCtx.StartTime)
		overheadDuration := totalDuration - reqCtx.UpstreamDuration
		if overheadDuration < 0 {
			overheadDuration = 0
		}

		tenantID := "unknown"
		if reqCtx.Tenant != nil && reqCtx.Tenant.ID != "" {
			tenantID = reqCtx.Tenant.ID
		}
		routePath := r.URL.Path
		modelName := "unknown"
		if reqCtx.ChatRequest != nil && reqCtx.ChatRequest.Model != "" {
			modelName = reqCtx.ChatRequest.Model
		}

		// 1. Record Prometheus Request Metrics
		if s.metrics != nil {
			s.metrics.RecordRequest(
				tenantID,
				routePath,
				strconv.Itoa(statusCode),
				modelName,
				totalDuration.Seconds(),
				overheadDuration.Seconds(),
			)

			// Record tokens
			promptTokens := 0
			completionTokens := 0
			if reqCtx.ChatResponse != nil && reqCtx.ChatResponse.Usage != nil {
				promptTokens = reqCtx.ChatResponse.Usage.PromptTokens
				completionTokens = reqCtx.ChatResponse.Usage.CompletionTokens
			}
			s.metrics.RecordTokens(tenantID, modelName, promptTokens, completionTokens)
		}

		// 2. Append to Cryptographically Chained Audit Ledger
		if s.auditLedger != nil {
			promptHash := reqCtx.PromptHash
			if promptHash == "" && reqCtx.ChatRequest != nil {
				promptHash = audit.HashPrompt(reqCtx.ChatRequest.Messages)
			}
			responseHash := reqCtx.ResponseHash
			if responseHash == "" && reqCtx.ChatResponse != nil {
				responseHash = audit.HashResponse(reqCtx.ChatResponse)
			}

			promptTokens := 0
			completionTokens := 0
			if reqCtx.ChatResponse != nil && reqCtx.ChatResponse.Usage != nil {
				promptTokens = reqCtx.ChatResponse.Usage.PromptTokens
				completionTokens = reqCtx.ChatResponse.Usage.CompletionTokens
			}

			policyAction := reqCtx.PolicyAction
			if policyAction == "" {
				policyAction = "ALLOW"
			}

			_, _ = s.auditLedger.Append(audit.Record{
				Timestamp:        reqCtx.StartTime.UTC().Format(time.RFC3339Nano),
				RequestID:        reqCtx.RequestID,
				TraceID:          reqCtx.TraceID,
				TenantID:         tenantID,
				Route:            routePath,
				UpstreamID:       reqCtx.UpstreamID,
				Model:            modelName,
				PromptHash:       promptHash,
				ResponseHash:     responseHash,
				TokensPrompt:     promptTokens,
				TokensCompletion: completionTokens,
				StatusCode:       statusCode,
				PolicyAction:     policyAction,
				OverheadMs:       float64(overheadDuration.Microseconds()) / 1000.0,
			})
		}
	}()

	reqCtx.SkipSyncWrite = s.pipeline != nil && s.pipeline.HasStage("post_policy")
	execErr = s.pipeline.Execute(reqCtx)
	if execErr != nil {
		s.handleError(w, r, execErr)
		return
	}
}

// handleListModels returns OpenAI-compatible list of available models filtered by tenant.
func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
	traceID, spanID := tracing.ExtractOrGenerate(r.Header.Get(tracing.TraceparentHeader))
	reqID := r.Header.Get(tracing.RequestIDHeader)
	if reqID == "" {
		reqID = tracing.NewRequestID()
	}
	w.Header().Set(tracing.TraceparentHeader, tracing.FormatTraceparent(traceID, spanID))
	w.Header().Set(tracing.RequestIDHeader, reqID)

	reqCtx := pipeline.NewRequestContext(r.Context(), w, r)
	reqCtx.TraceID = traceID
	reqCtx.SpanID = spanID
	reqCtx.RequestID = reqID

	// Authenticate caller first
	if err := s.authStage.Execute(reqCtx); err != nil {
		s.handleError(w, r, err)
		return
	}

	routes := s.cfg.ListRoutes()
	var modelsList []model.ModelListItem

	for _, route := range routes {
		if auth.IsRouteAllowed(reqCtx.Tenant.AllowedRoutes, route.Alias) {
			modelsList = append(modelsList, model.ModelListItem{
				ID:      route.Alias,
				Object:  "model",
				Created: time.Now().Unix(),
				OwnedBy: "company-ai-gateway",
			})
		}
	}

	resp := model.ModelListResponse{
		Object: "list",
		Data:   modelsList,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// handleLiveness provides an instant probe indicating process viability.
func (s *Server) handleLiveness(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// handleReadiness provides a probe indicating ready status to receive traffic.
// If all upstreams are failing or circuit breakers are OPEN, returns HTTP 503.
func (s *Server) handleReadiness(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if s.healthChecker != nil && !s.healthChecker.IsReady() {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status":"unhealthy","error":"ALL_UPSTREAMS_UNAVAILABLE"}`))
		return
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ready"}`))
}

// handleError renders structured, machine-readable JSON errors.
func (s *Server) handleError(w http.ResponseWriter, r *http.Request, err error) {
	// If the downstream client severed connection, avoid writing to dead socket
	if errors.Is(err, context.Canceled) || errors.Is(r.Context().Err(), context.Canceled) {
		s.logger.Warn("Downstream client canceled connection", "url", r.URL.Path)
		return
	}

	var gwErr *model.GatewayError
	if !errors.As(err, &gwErr) {
		gwErr = model.NewGatewayError(
			http.StatusInternalServerError,
			model.ErrCodeInternalError,
			err.Error(),
		)
	}

	s.logger.Warn("Request failed",
		"status", gwErr.StatusCode,
		"code", gwErr.Code,
		"message", gwErr.Message,
		"path", r.URL.Path,
	)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(gwErr.StatusCode)
	_, _ = w.Write(gwErr.ToJSON())
}

// loggingMiddleware records incoming request details and duration.
func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		wrapped := &responseStatusRecorder{ResponseWriter: w, statusCode: http.StatusOK}

		next.ServeHTTP(wrapped, r)

		duration := time.Since(start)
		s.logger.Info("HTTP Request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", wrapped.statusCode,
			"duration_ms", duration.Milliseconds(),
			"remote_addr", r.RemoteAddr,
		)
	})
}

// responseStatusRecorder tracks response status code for logging.
type responseStatusRecorder struct {
	http.ResponseWriter
	statusCode int
}

func (r *responseStatusRecorder) WriteHeader(code int) {
	r.statusCode = code
	r.ResponseWriter.WriteHeader(code)
}

// Flush implements http.Flusher by forwarding call to wrapped writer if supported.
func (r *responseStatusRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}
