package policy

import (
	"fmt"
	"net/http"

	"github.com/company/ai-gateway/internal/config"
	"github.com/company/ai-gateway/internal/model"
	"github.com/company/ai-gateway/internal/pipeline"
)

// PrePolicyStage scans incoming user prompts against bound policies before route dispatch.
type PrePolicyStage struct {
	engine *Engine
	cfg    *config.Config
}

// NewPrePolicyStage constructs a new PrePolicyStage.
func NewPrePolicyStage(engine *Engine, cfg ...*config.Config) *PrePolicyStage {
	if engine == nil {
		engine = NewEngine()
	}
	var c *config.Config
	if len(cfg) > 0 {
		c = cfg[0]
	}
	return &PrePolicyStage{
		engine: engine,
		cfg:    c,
	}
}

// Name returns the identifier of this stage.
func (p *PrePolicyStage) Name() string {
	return "pre_policy"
}

// Execute inspects all messages in the chat request.
func (p *PrePolicyStage) Execute(reqCtx *pipeline.RequestContext) error {
	if reqCtx.ChatRequest == nil {
		return nil
	}

	// Resolve policies from tenant bindings if not already populated
	if len(reqCtx.Policies) == 0 && reqCtx.Tenant != nil && p.cfg != nil {
		for _, pb := range reqCtx.Tenant.PolicyBindings {
			if pol, ok := p.cfg.GetPolicy(pb); ok {
				reqCtx.Policies = append(reqCtx.Policies, pol)
			}
		}
	}

	if len(reqCtx.Policies) == 0 {
		return nil
	}

	for i := range reqCtx.ChatRequest.Messages {
		msg := &reqCtx.ChatRequest.Messages[i]
		contentStr := msg.ContentString()
		if contentStr == "" {
			continue
		}

		res, err := p.engine.Scan(contentStr, reqCtx.Policies)
		if err != nil {
			return model.NewGatewayError(
				http.StatusInternalServerError,
				model.ErrCodeInternalError,
				fmt.Sprintf("Failed to execute DLP policy scan: %v", err),
			)
		}

		if res.Violated {
			if res.Action == "BLOCK" {
				return model.NewGatewayError(
					http.StatusBadRequest,
					model.ErrCodePolicyViolation,
					fmt.Sprintf("Request rejected due to policy violation: %s", res.ViolatedPolicy),
				)
			}
			// MASK / REDACT in place
			msg.Content = res.RedactedText
		}
	}

	return nil
}
