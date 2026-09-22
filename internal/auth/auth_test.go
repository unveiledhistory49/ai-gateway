package auth

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/company/ai-gateway/internal/config"
	"github.com/company/ai-gateway/internal/model"
	"github.com/company/ai-gateway/internal/pipeline"
)

func sampleTenants() []config.TenantConfig {
	return []config.TenantConfig{
		{
			ID:            "tenant-prod",
			Name:          "Production Service",
			Tier:          "production",
			APIKeys:       []string{"sk-gw-prod-live-key", "sk-gw-prod-secondary"},
			AllowedRoutes: []string{"gpt-4o", "gpt-4o-mini"},
			RateLimits: model.RateLimits{
				RequestsPerMinute: 600,
				TokensPerMinute:   500000,
				MaxConcurrent:     20,
			},
		},
		{
			ID:            "tenant-internal",
			Name:          "Internal Team",
			Tier:          "internal",
			APIKeys:       []string{"sk-gw-internal-wildcard"},
			AllowedRoutes: []string{"*"},
		},
	}
}

func TestAuthMissingHeader(t *testing.T) {
	stage := NewStage(sampleTenants(), 1024*1024)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"gpt-4o"}`))
	reqCtx := pipeline.NewRequestContext(context.Background(), w, r)

	err := stage.Execute(reqCtx)
	if err == nil {
		t.Fatal("expected error for missing header, got nil")
	}

	var gwErr *model.GatewayError
	if !errors.As(err, &gwErr) {
		t.Fatalf("expected GatewayError, got %T", err)
	}

	if gwErr.StatusCode != http.StatusUnauthorized || gwErr.Code != model.ErrCodeMissingOrInvalidAPIKey {
		t.Fatalf("unexpected error status/code: %d / %s", gwErr.StatusCode, gwErr.Code)
	}
}

func TestAuthInvalidKey(t *testing.T) {
	stage := NewStage(sampleTenants(), 1024*1024)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"gpt-4o"}`))
	r.Header.Set("Authorization", "Bearer sk-gw-fake-invalid-key")
	reqCtx := pipeline.NewRequestContext(context.Background(), w, r)

	err := stage.Execute(reqCtx)
	if err == nil {
		t.Fatal("expected error for invalid key, got nil")
	}

	var gwErr *model.GatewayError
	if !errors.As(err, &gwErr) {
		t.Fatalf("expected GatewayError, got %T", err)
	}

	if gwErr.StatusCode != http.StatusUnauthorized || gwErr.Code != model.ErrCodeMissingOrInvalidAPIKey {
		t.Fatalf("unexpected error status/code: %d / %s", gwErr.StatusCode, gwErr.Code)
	}
}

func TestAuthValidKeySuccess(t *testing.T) {
	stage := NewStage(sampleTenants(), 1024*1024)

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	r.Header.Set("Authorization", "Bearer sk-gw-prod-live-key")
	reqCtx := pipeline.NewRequestContext(context.Background(), w, r)

	err := stage.Execute(reqCtx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if reqCtx.Tenant == nil || reqCtx.Tenant.ID != "tenant-prod" {
		t.Fatalf("expected tenant-prod, got %v", reqCtx.Tenant)
	}

	if reqCtx.ChatRequest == nil || reqCtx.ChatRequest.Model != "gpt-4o" {
		t.Fatalf("expected chat request with model gpt-4o, got %v", reqCtx.ChatRequest)
	}
}

func TestAuthForbiddenRoute(t *testing.T) {
	stage := NewStage(sampleTenants(), 1024*1024)

	body := `{"model":"unauthorized-claude-model","messages":[{"role":"user","content":"hello"}]}`
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	r.Header.Set("Authorization", "Bearer sk-gw-prod-live-key")
	reqCtx := pipeline.NewRequestContext(context.Background(), w, r)

	err := stage.Execute(reqCtx)
	if err == nil {
		t.Fatal("expected error for unauthorized route, got nil")
	}

	var gwErr *model.GatewayError
	if !errors.As(err, &gwErr) {
		t.Fatalf("expected GatewayError, got %T", err)
	}

	if gwErr.StatusCode != http.StatusForbidden || gwErr.Code != model.ErrCodeForbiddenRoute {
		t.Fatalf("unexpected error: %d / %s", gwErr.StatusCode, gwErr.Code)
	}
}

func TestAuthWildcardRoute(t *testing.T) {
	stage := NewStage(sampleTenants(), 1024*1024)

	body := `{"model":"any-custom-model","messages":[{"role":"user","content":"hello"}]}`
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	r.Header.Set("Authorization", "Bearer sk-gw-internal-wildcard")
	reqCtx := pipeline.NewRequestContext(context.Background(), w, r)

	err := stage.Execute(reqCtx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if reqCtx.Tenant == nil || reqCtx.Tenant.ID != "tenant-internal" {
		t.Fatalf("expected tenant-internal, got %v", reqCtx.Tenant)
	}
}

func TestAuthInvalidJSONBody(t *testing.T) {
	stage := NewStage(sampleTenants(), 1024*1024)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{invalid-json}`))
	r.Header.Set("Authorization", "Bearer sk-gw-prod-live-key")
	reqCtx := pipeline.NewRequestContext(context.Background(), w, r)

	err := stage.Execute(reqCtx)
	if err == nil {
		t.Fatal("expected error for invalid json, got nil")
	}

	var gwErr *model.GatewayError
	if !errors.As(err, &gwErr) {
		t.Fatalf("expected GatewayError, got %T", err)
	}

	if gwErr.StatusCode != http.StatusBadRequest || gwErr.Code != model.ErrCodeBadRequest {
		t.Fatalf("unexpected error: %d / %s", gwErr.StatusCode, gwErr.Code)
	}
}

func TestVerifyKeyConstantTime(t *testing.T) {
	if !VerifyKeyConstantTime("secret-key-1", "secret-key-1") {
		t.Fatal("expected matching keys to return true")
	}
	if VerifyKeyConstantTime("secret-key-1", "secret-key-2") {
		t.Fatal("expected differing keys to return false")
	}
	if VerifyKeyConstantTime("secret-key-1", "secret-key-1-extended") {
		t.Fatal("expected differing length keys to return false")
	}
}
