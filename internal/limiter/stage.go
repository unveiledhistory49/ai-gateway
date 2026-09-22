package limiter

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/company/ai-gateway/internal/model"
	"github.com/company/ai-gateway/internal/pipeline"
)

// QuotaStage implements Stage 2: Token-Bucket & Quota Rate Limiting.
type QuotaStage struct {
	limiter *Limiter
}

// NewQuotaStage constructs a new QuotaStage.
func NewQuotaStage(limiter *Limiter) *QuotaStage {
	if limiter == nil {
		limiter = NewLimiter()
	}
	return &QuotaStage{
		limiter: limiter,
	}
}

// Name returns the identifier of this stage.
func (q *QuotaStage) Name() string {
	return "quota_limiter"
}

// Limiter returns the underlying Limiter instance.
func (q *QuotaStage) Limiter() *Limiter {
	return q.limiter
}

// Execute performs rate limit checks (RPM, TPM, Concurrency) and establishes two-phase token reservation.
func (q *QuotaStage) Execute(reqCtx *pipeline.RequestContext) error {
	if reqCtx.Tenant == nil {
		return nil
	}

	limits := reqCtx.Tenant.RateLimits

	// Phase 1 (Pre-Dispatch): Speculatively reserve estimated tokens:
	// Tokens_est = (len(prompt) / 4) + max_tokens (default max_tokens = 500 if not specified)
	var promptBuilder strings.Builder
	if reqCtx.ChatRequest != nil {
		for _, msg := range reqCtx.ChatRequest.Messages {
			promptBuilder.WriteString(msg.ContentString())
		}
	}
	prompt := promptBuilder.String()
	promptTokensEst := len(prompt) / 4

	maxTokens := 500
	if reqCtx.ChatRequest != nil && reqCtx.ChatRequest.MaxTokens != nil && *reqCtx.ChatRequest.MaxTokens > 0 {
		maxTokens = *reqCtx.ChatRequest.MaxTokens
	}
	estTokens := promptTokensEst + maxTokens

	res, retryAfter, err := q.limiter.Acquire(reqCtx.Tenant.ID, limits, estTokens)
	if err != nil {
		if retryAfter > 0 && reqCtx.Writer != nil {
			reqCtx.Writer.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		}
		var gwErr *model.GatewayError
		if ok := isGatewayError(err, &gwErr); ok {
			return gwErr
		}
		return model.NewGatewayError(
			http.StatusTooManyRequests,
			model.ErrCodeRateLimitExceeded,
			fmt.Sprintf("Rate limit exceeded: %v", err),
		)
	}

	// Register deferred reconciliation callback in RequestContext
	reqCtx.Reconcile = func(actualTokens int) {
		res.Reconcile(actualTokens)
	}

	return nil
}

func isGatewayError(err error, target **model.GatewayError) bool {
	if ge, ok := err.(*model.GatewayError); ok {
		*target = ge
		return true
	}
	return false
}
