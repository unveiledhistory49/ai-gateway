package router

import (
	"fmt"
	"net/http"

	"github.com/company/ai-gateway/internal/config"
	"github.com/company/ai-gateway/internal/model"
	"github.com/company/ai-gateway/internal/pipeline"
	"github.com/company/ai-gateway/internal/resilience"
)

// Stage implements Stage 2: Resilience-Aware Route Resolution.
type Stage struct {
	cfg      *config.Config
	registry *resilience.Registry
}

// NewStage creates a new route resolution Stage with optional Circuit Breaker registry.
func NewStage(cfg *config.Config, registry ...*resilience.Registry) *Stage {
	var reg *resilience.Registry
	if len(registry) > 0 {
		reg = registry[0]
	}
	return &Stage{
		cfg:      cfg,
		registry: reg,
	}
}

// Name returns the identifier of this stage.
func (s *Stage) Name() string {
	return "route_resolution"
}

// Execute resolves the requested model alias to an ordered cascade of healthy targets.
// It filters out targets whose circuit breakers are OPEN, selecting the highest-priority
// target that is Closed or a valid HalfOpen probe.
// If all targets for a route are OPEN, returns HTTP 503 CIRCUIT_OPEN.
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

	var healthyTargets []*model.RouteTarget

	// Tiers are sorted by Priority ascending (Tier 0 = Primary, Tier 1 = Fallback, Tier 2 = Emergency)
	for _, tier := range route.Tiers {
		for _, targetCfg := range tier.Targets {
			upstream, ok := s.cfg.GetUpstream(targetCfg.UpstreamID)
			if !ok {
				continue
			}

			// Check circuit breaker status: skip targets whose breaker is OPEN
			if s.registry != nil {
				cb := s.registry.Get(targetCfg.UpstreamID)
				if cb.State() == resilience.StateOpen {
					// Circuit breaker is OPEN; skip this target
					continue
				}
			}

			healthyTargets = append(healthyTargets, &model.RouteTarget{
				UpstreamID:   targetCfg.UpstreamID,
				Provider:     upstream.Provider,
				EndpointURL:  upstream.EndpointURL,
				TargetModel:  targetCfg.Model,
				APIKey:       upstream.APIKey,
				Timeout:      upstream.Timeout(),
				PriorityTier: tier.Priority,
				Weight:       targetCfg.Weight,
			})
		}
	}

	// If all configured targets have OPEN circuit breakers:
	if len(healthyTargets) == 0 {
		return model.NewGatewayError(
			http.StatusServiceUnavailable,
			model.ErrCodeCircuitOpen,
			fmt.Sprintf("All upstream targets for route '%s' are unavailable (CIRCUIT_OPEN / ALL_UPSTREAMS_UNAVAILABLE).", modelAlias),
		)
	}

	reqCtx.Route = route
	reqCtx.Targets = healthyTargets
	reqCtx.TargetModel = healthyTargets[0].TargetModel
	reqCtx.Upstream, _ = s.cfg.GetUpstream(healthyTargets[0].UpstreamID)

	return nil
}
