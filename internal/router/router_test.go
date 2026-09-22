package router

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/company/ai-gateway/internal/config"
	"github.com/company/ai-gateway/internal/model"
	"github.com/company/ai-gateway/internal/pipeline"
)

func setupTestConfig(t *testing.T) *config.Config {
	yamlData := `
server:
  port: 8080
upstreams:
  - id: "openai-prod"
    provider: "openai"
    endpoint_url: "https://api.openai.com"
    api_key: "sk-vaulted-key"
routes:
  - alias: "gpt-4o"
    primary_upstream: "openai-prod"
    model_name: "gpt-4o-2024-08-06"
  - alias: "fast-chat"
    primary_upstream: "openai-prod"
    model_name: "gpt-4o-mini"
tenants:
  - id: "t1"
    api_keys: ["k1"]
    allowed_routes: ["*"]
`
	cfg, err := config.ParseConfig(yamlData)
	if err != nil {
		t.Fatalf("failed to parse test config: %v", err)
	}
	return cfg
}

func TestRouteResolutionSuccess(t *testing.T) {
	cfg := setupTestConfig(t)
	stage := NewStage(cfg)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	reqCtx := pipeline.NewRequestContext(context.Background(), w, r)
	reqCtx.ChatRequest = &model.CanonicalChatRequest{
		Model: "fast-chat",
	}

	err := stage.Execute(reqCtx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if reqCtx.Route == nil || reqCtx.Route.Alias != "fast-chat" {
		t.Fatalf("expected route fast-chat, got %v", reqCtx.Route)
	}
	if reqCtx.Upstream == nil || reqCtx.Upstream.ID != "openai-prod" {
		t.Fatalf("expected upstream openai-prod, got %v", reqCtx.Upstream)
	}
	if reqCtx.TargetModel != "gpt-4o-mini" {
		t.Fatalf("expected TargetModel 'gpt-4o-mini', got '%s'", reqCtx.TargetModel)
	}
}

func TestRouteResolutionUnknownRoute(t *testing.T) {
	cfg := setupTestConfig(t)
	stage := NewStage(cfg)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	reqCtx := pipeline.NewRequestContext(context.Background(), w, r)
	reqCtx.ChatRequest = &model.CanonicalChatRequest{
		Model: "unmapped-model-alias",
	}

	err := stage.Execute(reqCtx)
	if err == nil {
		t.Fatal("expected error for unmapped route, got nil")
	}

	var gwErr *model.GatewayError
	if !errors.As(err, &gwErr) {
		t.Fatalf("expected GatewayError, got %T", err)
	}

	if gwErr.StatusCode != http.StatusNotFound || gwErr.Code != model.ErrCodeUnknownRoute {
		t.Fatalf("unexpected error: %d / %s", gwErr.StatusCode, gwErr.Code)
	}
}

func TestRouteResolutionMissingChatRequest(t *testing.T) {
	cfg := setupTestConfig(t)
	stage := NewStage(cfg)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	reqCtx := pipeline.NewRequestContext(context.Background(), w, r)

	err := stage.Execute(reqCtx)
	if err == nil {
		t.Fatal("expected error for missing chat request, got nil")
	}

	var gwErr *model.GatewayError
	if !errors.As(err, &gwErr) {
		t.Fatalf("expected GatewayError, got %T", err)
	}

	if gwErr.StatusCode != http.StatusBadRequest {
		t.Fatalf("unexpected status: %d", gwErr.StatusCode)
	}
}
