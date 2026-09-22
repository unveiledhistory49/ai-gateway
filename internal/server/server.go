package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/company/ai-gateway/internal/auth"
	"github.com/company/ai-gateway/internal/config"
	"github.com/company/ai-gateway/internal/dispatcher"
	"github.com/company/ai-gateway/internal/model"
	"github.com/company/ai-gateway/internal/pipeline"
	"github.com/company/ai-gateway/internal/router"
)

// Server encapsulates the HTTP ingress server, routing table, and processing pipeline.
type Server struct {
	cfg        *config.Config
	pipeline   *pipeline.Pipeline
	authStage  *auth.Stage
	httpServer *http.Server
	logger     *slog.Logger
}

// NewServer initializes a new Server using standard production stages.
func NewServer(cfg *config.Config, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}

	authStage := auth.NewStage(cfg.Tenants, cfg.Server.MaxBodyBytes)
	routeStage := router.NewStage(cfg)
	dispStage := dispatcher.NewStage(30 * time.Second)

	pipe := pipeline.NewPipeline(authStage, routeStage, dispStage)

	return NewServerWithPipeline(cfg, pipe, authStage, logger)
}

// NewServerWithPipeline constructs a Server with a custom pipeline (useful for testing).
func NewServerWithPipeline(cfg *config.Config, pipe *pipeline.Pipeline, authStage *auth.Stage, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}

	s := &Server{
		cfg:       cfg,
		pipeline:  pipe,
		authStage: authStage,
		logger:    logger,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("GET /v1/models", s.handleListModels)
	mux.HandleFunc("GET /healthz/liveness", s.handleLiveness)
	mux.HandleFunc("GET /healthz/readiness", s.handleReadiness)

	s.httpServer = &http.Server{
		Addr:         fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port),
		Handler:      s.loggingMiddleware(mux),
		ReadTimeout:  cfg.Server.ReadTimeout(),
		WriteTimeout: cfg.Server.WriteTimeout(),
		IdleTimeout:  cfg.Server.IdleTimeout(),
	}

	return s
}

// Start runs the HTTP server listener.
func (s *Server) Start() error {
	s.logger.Info("Starting AI Gateway HTTP Ingress Server",
		"host", s.cfg.Server.Host,
		"port", s.cfg.Server.Port,
	)
	if err := s.httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("HTTP ingress listener error: %w", err)
	}
	return nil
}

// Shutdown gracefully terminates in-flight connections within the provided context deadline.
func (s *Server) Shutdown(ctx context.Context) error {
	s.logger.Info("Draining in-flight connections and shutting down HTTP Ingress Server...")
	return s.httpServer.Shutdown(ctx)
}

// Handler exposes the internal http.Handler for testing purposes.
func (s *Server) Handler() http.Handler {
	return s.httpServer.Handler
}

// handleChatCompletions processes inbound OpenAI-compatible chat completion requests.
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	reqCtx := pipeline.NewRequestContext(r.Context(), w, r)

	err := s.pipeline.Execute(reqCtx)
	if err != nil {
		s.handleError(w, r, err)
		return
	}
}

// handleListModels returns OpenAI-compatible list of available models filtered by tenant.
func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
	reqCtx := pipeline.NewRequestContext(r.Context(), w, r)

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
func (s *Server) handleReadiness(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
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
