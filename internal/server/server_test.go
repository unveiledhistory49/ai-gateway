package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/company/ai-gateway/internal/config"
	"github.com/company/ai-gateway/internal/model"
)

func setupTestServer(t *testing.T) *Server {
	rawYAML := `
server:
  host: "127.0.0.1"
  port: 8080
upstreams:
  - id: "up-mock"
    endpoint_url: "https://mock.ai"
routes:
  - alias: "gpt-4o"
    primary_upstream: "up-mock"
    model_name: "gpt-4o"
  - alias: "prod-fast"
    primary_upstream: "up-mock"
    model_name: "gpt-4o-mini"
  - alias: "internal-restricted"
    primary_upstream: "up-mock"
    model_name: "llama-3-70b"
tenants:
  - id: "tenant-normal"
    api_keys: ["sk-normal-key"]
    allowed_routes: ["gpt-4o", "prod-fast"]
  - id: "tenant-admin"
    api_keys: ["sk-admin-key"]
    allowed_routes: ["*"]
`
	cfg, err := config.ParseConfig(rawYAML)
	if err != nil {
		t.Fatalf("failed to parse config: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewServer(cfg, logger)
}

func TestHealthEndpoints(t *testing.T) {
	srv := setupTestServer(t)

	// Liveness
	req := httptest.NewRequest(http.MethodGet, "/healthz/liveness", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("liveness: expected 200, got %d", rec.Code)
	}
	if rec.Body.String() != `{"status":"ok"}` {
		t.Fatalf("liveness: unexpected body %s", rec.Body.String())
	}

	// Readiness
	req = httptest.NewRequest(http.MethodGet, "/healthz/readiness", nil)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("readiness: expected 200, got %d", rec.Code)
	}
	if rec.Body.String() != `{"status":"ready"}` {
		t.Fatalf("readiness: unexpected body %s", rec.Body.String())
	}
}

func TestListModelsUnauthorized(t *testing.T) {
	srv := setupTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized, got %d", rec.Code)
	}

	var errResp model.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("failed to parse error json: %v", err)
	}
	if errResp.Error.Code != model.ErrCodeMissingOrInvalidAPIKey {
		t.Fatalf("expected code %s, got %s", model.ErrCodeMissingOrInvalidAPIKey, errResp.Error.Code)
	}
}

func TestListModelsFilteredByTenant(t *testing.T) {
	srv := setupTestServer(t)

	// Normal tenant only allows gpt-4o and prod-fast (not internal-restricted)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-normal-key")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", rec.Code)
	}

	var listResp model.ModelListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("failed to parse model list json: %v", err)
	}

	if len(listResp.Data) != 2 {
		t.Fatalf("expected 2 models for tenant-normal, got %d", len(listResp.Data))
	}
	for _, m := range listResp.Data {
		if m.ID == "internal-restricted" {
			t.Fatalf("tenant-normal should not have access to internal-restricted model")
		}
	}

	// Admin tenant allows wildcard "*"
	reqAdmin := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	reqAdmin.Header.Set("Authorization", "Bearer sk-admin-key")
	recAdmin := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recAdmin, reqAdmin)

	if recAdmin.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for admin, got %d", recAdmin.Code)
	}

	var adminListResp model.ModelListResponse
	if err := json.Unmarshal(recAdmin.Body.Bytes(), &adminListResp); err != nil {
		t.Fatalf("failed to parse admin model list: %v", err)
	}

	if len(adminListResp.Data) != 3 {
		t.Fatalf("expected all 3 models for admin, got %d", len(adminListResp.Data))
	}
}

func TestReadinessWhenUpstreamsDown(t *testing.T) {
	srv := setupTestServer(t)

	// Trip the circuit breaker for up-mock
	cb := srv.Registry().Get("up-mock")
	// Set min requests to 1 and record failure
	cb.Reset()
	for i := 0; i < 20; i++ {
		cb.RecordFailure()
	}

	req := httptest.NewRequest(http.MethodGet, "/healthz/readiness", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 Service Unavailable when breaker is open, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ALL_UPSTREAMS_UNAVAILABLE") {
		t.Fatalf("expected error message to contain ALL_UPSTREAMS_UNAVAILABLE, got: %s", rec.Body.String())
	}
}

