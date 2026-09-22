package router

import (
	"fmt"
	"net/http"

	"github.com/company/ai-gateway/internal/config"
	"github.com/company/ai-gateway/internal/model"
	"github.com/company/ai-gateway/internal/pipeline"
)

// Stage implements Stage 2: Route Resolution.
type Stage struct {
	cfg *config.Config
}

// NewStage creates a new route resolution Stage.
func NewStage(cfg *config.Config) *Stage {
	return &Stage{
		cfg: cfg,
	}
}

// Name returns the identifier of this stage.
func (s *Stage) Name() string {
	return "route_resolution"
}

// Execute resolves the requested model alias to an active RouteConfig and UpstreamConfig.
func (s *Stage) Execute(reqCtx *pipeline.RequestContext) error {
	if reqCtx.ChatRequest == nil {
		return model.NewGatewayError(
			http.StatusBadRequest,
			model.ErrCodeBadRequest,
			"Cannot resolve route: no chat request parsed.",
		)
	}

	modelAlias := reqCtx.ChatRequest.Model
	route, found := s.cfg.GetRoute(modelAlias)
	if !found {
		return model.NewGatewayError(
			http.StatusNotFound,
			model.ErrCodeUnknownRoute,
			fmt.Sprintf("Unknown model or route: '%s'. Check /v1/models for available routes.", modelAlias),
		)
	}

	upstream, found := s.cfg.GetUpstream(route.PrimaryUpstream)
	if !found {
		return model.NewGatewayError(
			http.StatusBadGateway,
			model.ErrCodeUpstreamBadGateway,
			fmt.Sprintf("Primary upstream '%s' for route '%s' is not configured.", route.PrimaryUpstream, modelAlias),
		)
	}

	reqCtx.Route = route
	reqCtx.Upstream = upstream
	reqCtx.TargetModel = route.ModelName

	return nil
}
