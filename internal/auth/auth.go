package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/company/ai-gateway/internal/config"
	"github.com/company/ai-gateway/internal/model"
	"github.com/company/ai-gateway/internal/pipeline"
)

// Stage implements the authentication and tenant extraction stage.
type Stage struct {
	tenants      []config.TenantConfig
	maxBodyBytes int64
}

// NewStage creates a new authentication Stage.
func NewStage(tenants []config.TenantConfig, maxBodyBytes int64) *Stage {
	if maxBodyBytes <= 0 {
		maxBodyBytes = config.DefaultMaxBodyBytes
	}
	return &Stage{
		tenants:      tenants,
		maxBodyBytes: maxBodyBytes,
	}
}

// Name returns the identifier of this stage.
func (s *Stage) Name() string {
	return "auth_and_tenant_extraction"
}

// Execute performs constant-time token verification, establishes TenantContext,
// parses canonical chat request, and validates model/route authorization.
func (s *Stage) Execute(reqCtx *pipeline.RequestContext) error {
	authHeader := reqCtx.RawRequest.Header.Get("Authorization")
	if authHeader == "" || !strings.HasPrefix(authHeader, "Bearer ") {
		return model.NewGatewayError(
			http.StatusUnauthorized,
			model.ErrCodeMissingOrInvalidAPIKey,
			"Missing or invalid Authorization header. Expected Bearer token format: 'Bearer <api-key>'.",
		)
	}

	apiKey := strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
	if apiKey == "" {
		return model.NewGatewayError(
			http.StatusUnauthorized,
			model.ErrCodeMissingOrInvalidAPIKey,
			"API key in Authorization header is empty.",
		)
	}

	// Constant-time key comparison across tenants
	apiKeyHash := sha256.Sum256([]byte(apiKey))
	var matchedTenant *config.TenantConfig

	for i := range s.tenants {
		tenant := &s.tenants[i]
		for _, key := range tenant.APIKeys {
			candidateHash := sha256.Sum256([]byte(key))
			if subtle.ConstantTimeCompare(apiKeyHash[:], candidateHash[:]) == 1 {
				matchedTenant = tenant
			}
		}
	}

	if matchedTenant == nil {
		return model.NewGatewayError(
			http.StatusUnauthorized,
			model.ErrCodeMissingOrInvalidAPIKey,
			"Invalid API key provided.",
		)
	}

	reqCtx.Tenant = matchedTenant.ToTenantContext()

	// Parse body if it is a POST request and not yet parsed
	if reqCtx.ChatRequest == nil && reqCtx.RawRequest.Body != nil && reqCtx.RawRequest.Method == http.MethodPost {
		bodyBytes, err := io.ReadAll(http.MaxBytesReader(reqCtx.Writer, reqCtx.RawRequest.Body, s.maxBodyBytes))
		if err != nil {
			return model.NewGatewayError(
				http.StatusBadRequest,
				model.ErrCodeBadRequest,
				fmt.Sprintf("Failed to read request body: %v", err),
			)
		}
		if len(bodyBytes) == 0 {
			return model.NewGatewayError(
				http.StatusBadRequest,
				model.ErrCodeBadRequest,
				"Request body cannot be empty.",
			)
		}

		var chatReq model.CanonicalChatRequest
		if err := json.Unmarshal(bodyBytes, &chatReq); err != nil {
			return model.NewGatewayError(
				http.StatusBadRequest,
				model.ErrCodeBadRequest,
				fmt.Sprintf("Invalid JSON in request body: %v", err),
			)
		}
		reqCtx.ChatRequest = &chatReq
	}

	// Verify route permission if chat request is present
	if reqCtx.ChatRequest != nil {
		requestedModel := strings.TrimSpace(reqCtx.ChatRequest.Model)
		if requestedModel == "" {
			return model.NewGatewayError(
				http.StatusBadRequest,
				model.ErrCodeMissingModel,
				"The 'model' field is required in the chat completion request.",
			)
		}

		if !IsRouteAllowed(reqCtx.Tenant.AllowedRoutes, requestedModel) {
			return model.NewGatewayError(
				http.StatusForbidden,
				model.ErrCodeForbiddenRoute,
				fmt.Sprintf("Tenant '%s' is not authorized to access model '%s'.", reqCtx.Tenant.ID, requestedModel),
			)
		}
	}

	return nil
}

// IsRouteAllowed verifies if the requested model is permitted under the allowed routes.
func IsRouteAllowed(allowedRoutes []string, requestedModel string) bool {
	for _, route := range allowedRoutes {
		if route == "*" || route == requestedModel {
			return true
		}
	}
	return false
}

// VerifyKeyConstantTime checks two keys in constant time using SHA-256 hashes.
func VerifyKeyConstantTime(key1, key2 string) bool {
	h1 := sha256.Sum256([]byte(key1))
	h2 := sha256.Sum256([]byte(key2))
	return subtle.ConstantTimeCompare(h1[:], h2[:]) == 1
}
