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
	"github.com/company/ai-gateway/internal/resilience"
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

func TestRouteFallbackCascadeAndCircuitBreaker(t *testing.T) {
	yamlData := `
server:
  port: 8080
upstreams:
  - id: "openai-primary"
    endpoint_url: "https://api.openai.com"
  - id: "anthropic-backup"
    endpoint_url: "https://api.anthropic.com"
  - id: "vllm-local"
    endpoint_url: "http://127.0.0.1:8000"
routes:
  - alias: "prod-chat"
    tiers:
      - priority: 0
        targets:
          - upstream_id: "openai-primary"
            model: "gpt-4o"
      - priority: 1
        targets:
          - upstream_id: "anthropic-backup"
            model: "claude-3-5"
      - priority: 2
        targets:
          - upstream_id: "vllm-local"
            model: "llama-3"
tenants:
  - id: "t1"
    api_keys: ["k1"]
    allowed_routes: ["*"]
`
	cfg, err := config.ParseConfig(yamlData)
	if err != nil {
		t.Fatalf("failed to parse config: %v", err)
	}

	reg := resilience.NewRegistry(resilience.CircuitBreakerConfig{
		MinRequests: 1,
	}, nil)
	stage := NewStage(cfg, reg)

	// Helper to make request context
	makeReqCtx := func() *pipeline.RequestContext {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		reqCtx := pipeline.NewRequestContext(context.Background(), w, r)
		reqCtx.ChatRequest = &model.CanonicalChatRequest{Model: "prod-chat"}
		return reqCtx
	}

	// 1. All upstreams healthy: Primary Tier 0 selected
	reqCtx1 := makeReqCtx()
	if err := stage.Execute(reqCtx1); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reqCtx1.Upstream.ID != "openai-primary" {
		t.Fatalf("expected openai-primary (Tier 0), got %s", reqCtx1.Upstream.ID)
	}
	if len(reqCtx1.Targets) != 3 {
		t.Fatalf("expected 3 cascade targets, got %d", len(reqCtx1.Targets))
	}

	// 2. Trip Tier 0 breaker: Router must immediately select Tier 1 (anthropic-backup)
	cbPrimary := reg.Get("openai-primary")
	cbPrimary.RecordFailure() // Trips breaker because MinRequests=1
	if cbPrimary.State() != resilience.StateOpen {
		t.Fatalf("expected primary breaker to be OPEN, got %s", cbPrimary.State())
	}

	reqCtx2 := makeReqCtx()
	if err := stage.Execute(reqCtx2); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reqCtx2.Upstream.ID != "anthropic-backup" {
		t.Fatalf("expected fallback to anthropic-backup (Tier 1), got %s", reqCtx2.Upstream.ID)
	}
	if len(reqCtx2.Targets) != 2 {
		t.Fatalf("expected 2 remaining healthy targets, got %d", len(reqCtx2.Targets))
	}

	// 3. Trip Tier 1 breaker: Router selects Tier 2 (vllm-local)
	cbBackup := reg.Get("anthropic-backup")
	cbBackup.RecordFailure()
	if cbBackup.State() != resilience.StateOpen {
		t.Fatalf("expected backup breaker to be OPEN, got %s", cbBackup.State())
	}

	reqCtx3 := makeReqCtx()
	if err := stage.Execute(reqCtx3); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reqCtx3.Upstream.ID != "vllm-local" {
		t.Fatalf("expected emergency fallback to vllm-local (Tier 2), got %s", reqCtx3.Upstream.ID)
	}

	// 4. Trip Tier 2 breaker: All upstreams are OPEN -> router returns 503 CIRCUIT_OPEN
	cbEmergency := reg.Get("vllm-local")
	cbEmergency.RecordFailure()

	reqCtx4 := makeReqCtx()
	err4 := stage.Execute(reqCtx4)
	if err4 == nil {
		t.Fatal("expected 503 error when all targets are OPEN, got nil")
	}

	var gwErr *model.GatewayError
	if !errors.As(err4, &gwErr) {
		t.Fatalf("expected GatewayError, got %T", err4)
	}
	if gwErr.StatusCode != http.StatusServiceUnavailable || gwErr.Code != model.ErrCodeCircuitOpen {
		t.Fatalf("expected 503 CIRCUIT_OPEN, got %d / %s", gwErr.StatusCode, gwErr.Code)
	}
}

