package policy

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/company/ai-gateway/internal/model"
	"github.com/company/ai-gateway/internal/pipeline"
)

// PostPolicyStage scans non-streaming completion responses before delivery to downstream caller.
type PostPolicyStage struct {
	engine *Engine
}

// NewPostPolicyStage constructs a new PostPolicyStage.
func NewPostPolicyStage(engine *Engine) *PostPolicyStage {
	if engine == nil {
		engine = NewEngine()
	}
	return &PostPolicyStage{
		engine: engine,
	}
}

// Name returns the identifier of this stage.
func (p *PostPolicyStage) Name() string {
	return "post_policy"
}

// Execute inspects completion response choices and writes the final JSON response.
func (p *PostPolicyStage) Execute(reqCtx *pipeline.RequestContext) error {
	if reqCtx.ChatResponse == nil {
		return nil
	}

	// Scan completion choices if policies are bound
	if len(reqCtx.Policies) > 0 {
		for i := range reqCtx.ChatResponse.Choices {
			choice := &reqCtx.ChatResponse.Choices[i]
			contentStr := choice.Message.ContentString()
			if contentStr == "" {
				continue
			}

			res, err := p.engine.Scan(contentStr, reqCtx.Policies)
			if err != nil {
				return model.NewGatewayError(
					http.StatusInternalServerError,
					model.ErrCodeInternalError,
					fmt.Sprintf("Failed to execute post-dispatch DLP scan: %v", err),
				)
			}

			if res.Violated {
				if res.Action == "BLOCK" {
					// Discard reservation upon violation abort
					reqCtx.ExecuteReconcile(0)
					return model.NewGatewayError(
						http.StatusBadRequest,
						model.ErrCodePolicyViolation,
						fmt.Sprintf("Response rejected due to policy violation: %s", res.ViolatedPolicy),
					)
				}
				// MASK / REDACT
				choice.Message.Content = res.RedactedText
			}
		}
	}

	// Reconcile actual tokens with rate limiter
	actualTokens := 0
	if reqCtx.ChatResponse.Usage != nil {
		actualTokens = reqCtx.ChatResponse.Usage.TotalTokens
	}
	reqCtx.ExecuteReconcile(actualTokens)

	// Write response if not already written
	if reqCtx.Writer != nil && !reqCtx.ShortCircuited {
		bodyBytes, err := json.Marshal(reqCtx.ChatResponse)
		if err != nil {
			return model.NewGatewayError(
				http.StatusInternalServerError,
				model.ErrCodeInternalError,
				fmt.Sprintf("Failed to marshal chat completion response: %v", err),
			)
		}

		reqCtx.Writer.Header().Set("Content-Type", "application/json")
		reqCtx.Writer.WriteHeader(http.StatusOK)
		reqCtx.Writer.Write(bodyBytes)
		reqCtx.ShortCircuited = true
	}

	return nil
}
