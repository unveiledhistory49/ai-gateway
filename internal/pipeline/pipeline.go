package pipeline

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/company/ai-gateway/internal/config"
	"github.com/company/ai-gateway/internal/model"
)

// RequestContext encapsulates all state across the lifecycle of an in-flight request.
type RequestContext struct {
	Context context.Context

	// Downstream HTTP handles
	Writer     http.ResponseWriter
	RawRequest *http.Request

	// Parsed Chat Completion structures
	ChatRequest  *model.CanonicalChatRequest
	ChatResponse *model.CanonicalChatResponse

	// Tenant and Route resolution
	Tenant      *model.TenantContext
	Route       *config.RouteConfig
	Upstream    *config.UpstreamConfig
	TargetModel string
	Targets     []*model.RouteTarget
	Policies    []*config.PolicyConfig

	// Observability & stage timing
	StartTime      time.Time
	StageDurations map[string]time.Duration
	Metadata       map[string]any

	// Rate Limiting & Two-Phase Token Reservation
	Reconcile      func(actualTokens int)
	reconcileOnce  sync.Once

	// Control flags
	ShortCircuited bool
	SkipSyncWrite  bool
}

// ExecuteReconcile executes the deferred reconciliation callback exactly once.
func (r *RequestContext) ExecuteReconcile(actualTokens int) {
	r.reconcileOnce.Do(func() {
		if r.Reconcile != nil {
			r.Reconcile(actualTokens)
		}
	})
}

// NewRequestContext initializes a RequestContext for an incoming HTTP request.
func NewRequestContext(ctx context.Context, w http.ResponseWriter, r *http.Request) *RequestContext {
	return &RequestContext{
		Context:        ctx,
		Writer:         w,
		RawRequest:     r,
		StartTime:      time.Now(),
		StageDurations: make(map[string]time.Duration),
		Metadata:       make(map[string]any),
	}
}

// Stage represents a single discrete, ordered processing stage in the gateway pipeline.
type Stage interface {
	Name() string
	Execute(ctx *RequestContext) error
}

// Pipeline is the linear stage runner that executes stages sequentially.
type Pipeline struct {
	stages []Stage
}

// NewPipeline creates a new pipeline with the provided stages.
func NewPipeline(stages ...Stage) *Pipeline {
	return &Pipeline{
		stages: stages,
	}
}

// Stages returns a slice of the configured stages in order.
func (p *Pipeline) Stages() []Stage {
	return append([]Stage(nil), p.stages...)
}

// HasStage reports whether a stage with the given name is present in the pipeline.
func (p *Pipeline) HasStage(name string) bool {
	for _, stage := range p.stages {
		if stage.Name() == name {
			return true
		}
	}
	return false
}

// Execute runs all stages sequentially.
// It checks for context cancellation before each stage and tracks stage durations.
func (p *Pipeline) Execute(reqCtx *RequestContext) error {
	for _, stage := range p.stages {
		// Context check before running stage
		select {
		case <-reqCtx.Context.Done():
			return reqCtx.Context.Err()
		default:
		}

		start := time.Now()
		err := stage.Execute(reqCtx)
		duration := time.Since(start)
		reqCtx.StageDurations[stage.Name()] = duration

		if err != nil {
			return err
		}

		if reqCtx.ShortCircuited {
			// Pipeline short-circuited gracefully (e.g. response was served from cache)
			return nil
		}
	}
	return nil
}
