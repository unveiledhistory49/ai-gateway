package model

import (
	"encoding/json"
	"fmt"
	"time"
)

// TenantContext encapsulates the tenant identity, tier, quotas, and permissions
// resolved during authentication.
type TenantContext struct {
	ID             string            `json:"id" yaml:"id"`
	Name           string            `json:"name" yaml:"name"`
	Tier           string            `json:"tier" yaml:"tier"`
	AllowedRoutes  []string          `json:"allowed_routes" yaml:"allowed_routes"`
	RateLimits     RateLimits        `json:"rate_limits,omitempty" yaml:"rate_limits,omitempty"`
	PolicyBindings []string          `json:"policy_bindings,omitempty" yaml:"policy_bindings,omitempty"`
	Metadata       map[string]string `json:"metadata,omitempty" yaml:"metadata,omitempty"`
}

// RateLimits specifies request, token, and concurrency limits for a tenant.
type RateLimits struct {
	RequestsPerMinute int `json:"requests_per_minute,omitempty" yaml:"requests_per_minute,omitempty"`
	TokensPerMinute   int `json:"tokens_per_minute,omitempty" yaml:"tokens_per_minute,omitempty"`
	MaxConcurrent     int `json:"max_concurrent,omitempty" yaml:"max_concurrent,omitempty"`
}

// ChatMessage represents a single message in an OpenAI-compatible chat completion conversation.
type ChatMessage struct {
	Role         string `json:"role"`
	Content      any    `json:"content"` // string or structured multimodal content parts
	Name         string `json:"name,omitempty"`
	FunctionCall any    `json:"function_call,omitempty"`
	ToolCalls    any    `json:"tool_calls,omitempty"`
	ToolCallID   string `json:"tool_call_id,omitempty"`
}

// ContentString returns the string representation of message content if it is a string.
func (m *ChatMessage) ContentString() string {
	if str, ok := m.Content.(string); ok {
		return str
	}
	return ""
}

// CanonicalChatRequest models the incoming OpenAI /v1/chat/completions payload.
type CanonicalChatRequest struct {
	Model            string         `json:"model"`
	Messages         []ChatMessage  `json:"messages"`
	Stream           bool           `json:"stream,omitempty"`
	Temperature      *float64       `json:"temperature,omitempty"`
	TopP             *float64       `json:"top_p,omitempty"`
	N                *int           `json:"n,omitempty"`
	Stop             any            `json:"stop,omitempty"`
	MaxTokens        *int           `json:"max_tokens,omitempty"`
	PresencePenalty  *float64       `json:"presence_penalty,omitempty"`
	FrequencyPenalty *float64       `json:"frequency_penalty,omitempty"`
	User             string         `json:"user,omitempty"`
	Tools            any            `json:"tools,omitempty"`
	ToolChoice       any            `json:"tool_choice,omitempty"`
	ResponseFormat   any            `json:"response_format,omitempty"`
	Seed             *int           `json:"seed,omitempty"`
	StreamOptions    *StreamOptions `json:"stream_options,omitempty"`
}

// StreamOptions configures stream behavior such as including usage stats.
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

// ChatChoice represents an individual choice in a completion response.
type ChatChoice struct {
	Index        int         `json:"index"`
	Message      ChatMessage `json:"message"`
	FinishReason *string     `json:"finish_reason"`
	LogProbs     any         `json:"logprobs,omitempty"`
}

// UsageInfo represents token usage statistics.
type UsageInfo struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// CanonicalChatResponse models the response from an OpenAI-compatible /v1/chat/completions endpoint.
type CanonicalChatResponse struct {
	ID                string       `json:"id"`
	Object            string       `json:"object"`
	Created           int64        `json:"created"`
	Model             string       `json:"model"`
	SystemFingerprint string       `json:"system_fingerprint,omitempty"`
	Choices           []ChatChoice `json:"choices"`
	Usage             *UsageInfo   `json:"usage,omitempty"`
}

// StreamChoiceDelta holds incremental content deltas in a streaming chunk.
type StreamChoiceDelta struct {
	Role      string `json:"role,omitempty"`
	Content   string `json:"content,omitempty"`
	ToolCalls any    `json:"tool_calls,omitempty"`
}

// StreamChoice represents an individual choice within an SSE chunk.
type StreamChoice struct {
	Index        int               `json:"index"`
	Delta        StreamChoiceDelta `json:"delta"`
	FinishReason *string           `json:"finish_reason"`
	LogProbs     any               `json:"logprobs,omitempty"`
}

// StreamChunk represents an event stream chunk frame for Server-Sent Events (SSE).
type StreamChunk struct {
	ID                string         `json:"id"`
	Object            string         `json:"object"`
	Created           int64          `json:"created"`
	Model             string         `json:"model"`
	SystemFingerprint string         `json:"system_fingerprint,omitempty"`
	Choices           []StreamChoice `json:"choices"`
	Usage             *UsageInfo     `json:"usage,omitempty"`
}

// GatewayError represents a structured, machine-readable gateway error with an HTTP status code.
type GatewayError struct {
	StatusCode int    `json:"-"`
	Code       string `json:"code"`
	Message    string `json:"message"`
	Param      string `json:"param,omitempty"`
	Type       string `json:"type"`
}

func (e *GatewayError) Error() string {
	return fmt.Sprintf("[%s] %s (HTTP %d)", e.Code, e.Message, e.StatusCode)
}

// ErrorResponse wraps GatewayError to adhere to OpenAI API error conventions.
type ErrorResponse struct {
	Error GatewayError `json:"error"`
}

// Standard machine-readable error codes.
const (
	ErrCodeMissingOrInvalidAPIKey  = "MISSING_OR_INVALID_API_KEY"
	ErrCodeForbiddenRoute          = "FORBIDDEN_ROUTE"
	ErrCodeUnknownRoute            = "UNKNOWN_ROUTE"
	ErrCodeBadRequest              = "BAD_REQUEST"
	ErrCodeMissingModel            = "MISSING_MODEL"
	ErrCodeUpstreamUnavailable     = "UPSTREAM_UNAVAILABLE"
	ErrCodeUpstreamBadGateway      = "UPSTREAM_BAD_GATEWAY"
	ErrCodeUpstreamTimeout         = "UPSTREAM_TIMEOUT"
	ErrCodeInternalError           = "INTERNAL_ERROR"
	ErrCodeRateLimitExceeded       = "RATE_LIMIT_EXCEEDED"
	ErrCodeConcurrencyLimitExceeded = "CONCURRENCY_LIMIT_EXCEEDED"
	ErrCodePolicyViolation         = "POLICY_VIOLATION"
	ErrCodeCircuitOpen             = "CIRCUIT_OPEN"
	ErrCodeAllUpstreamsUnavailable = "ALL_UPSTREAMS_UNAVAILABLE"
)

// NewGatewayError constructs a new GatewayError.
func NewGatewayError(statusCode int, code string, message string) *GatewayError {
	errorType := "invalid_request_error"
	if statusCode >= 500 {
		errorType = "api_error"
	} else if statusCode == 401 {
		errorType = "authentication_error"
	} else if statusCode == 403 {
		errorType = "permission_error"
	} else if statusCode == 429 {
		errorType = "rate_limit_error"
	}

	return &GatewayError{
		StatusCode: statusCode,
		Code:       code,
		Message:    message,
		Type:       errorType,
	}
}

// ToJSON marshals the GatewayError into OpenAI-compatible JSON error envelope.
func (e *GatewayError) ToJSON() []byte {
	resp := ErrorResponse{
		Error: *e,
	}
	data, _ := json.Marshal(resp)
	return data
}

// ModelListResponse represents the response for GET /v1/models.
type ModelListResponse struct {
	Object string          `json:"object"`
	Data   []ModelListItem `json:"data"`
}

// ModelListItem represents a single model entry in GET /v1/models.
type ModelListItem struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// RouteTarget represents a physical upstream target evaluated in the fallback cascade.
type RouteTarget struct {
	UpstreamID   string        `json:"upstream_id"`
	Provider     string        `json:"provider"`
	EndpointURL  string        `json:"endpoint_url"`
	TargetModel  string        `json:"target_model"`
	APIKey       string        `json:"-"`
	Timeout      time.Duration `json:"timeout"`
	PriorityTier int           `json:"priority_tier"`
	Weight       int           `json:"weight"`
}

