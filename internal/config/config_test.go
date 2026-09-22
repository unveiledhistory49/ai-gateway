package config

import (
	"os"
	"strings"
	"testing"
)

func TestExpandEnv(t *testing.T) {
	os.Setenv("TEST_GATEWAY_PORT", "9090")
	os.Setenv("TEST_OPENAI_KEY", "sk-secret-12345")
	os.Unsetenv("TEST_UNSET_VAR")
	defer func() {
		os.Unsetenv("TEST_GATEWAY_PORT")
		os.Unsetenv("TEST_OPENAI_KEY")
	}()

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "existing env var",
			input:    "port: ${TEST_GATEWAY_PORT}",
			expected: "port: 9090",
		},
		{
			name:     "unset env var with :- default",
			input:    "key: ${TEST_UNSET_VAR:-default_key}",
			expected: "key: default_key",
		},
		{
			name:     "unset env var with : default",
			input:    "key: ${TEST_UNSET_VAR:fallback_key}",
			expected: "key: fallback_key",
		},
		{
			name:     "existing env var ignores default",
			input:    "key: ${TEST_OPENAI_KEY:-fallback}",
			expected: "key: sk-secret-12345",
		},
		{
			name:     "unset env var with no default",
			input:    "key: ${TEST_UNSET_VAR}",
			expected: "key: ",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			actual := ExpandEnv(tc.input)
			if actual != tc.expected {
				t.Fatalf("expected %q, got %q", tc.expected, actual)
			}
		})
	}
}

func TestValidConfig(t *testing.T) {
	yamlData := `
server:
  host: "127.0.0.1"
  port: 8080
  read_timeout_seconds: 10
  write_timeout_seconds: 45
  idle_timeout_seconds: 90
  max_body_bytes: 8388608

upstreams:
  - id: "openai-main"
    provider: "openai"
    endpoint_url: "https://api.openai.com"
    api_key: "sk-openai-secret"
    timeout_seconds: 25
  - id: "vllm-local"
    provider: "vllm"
    endpoint_url: "http://127.0.0.1:8000"
    timeout_seconds: 60

routes:
  - alias: "gpt-4o"
    primary_upstream: "openai-main"
    model_name: "gpt-4o"
  - alias: "internal-fast"
    primary_upstream: "vllm-local"
    model_name: "meta-llama/Llama-3-8b"

tenants:
  - id: "tenant-core"
    name: "Core Platform"
    tier: "production"
    api_keys:
      - "sk-gw-tenant-core-1"
      - "sk-gw-tenant-core-2"
    allowed_routes:
      - "gpt-4o"
      - "internal-fast"
    rate_limits:
      requests_per_minute: 1000
      tokens_per_minute: 2000000
      max_concurrent: 100
  - id: "tenant-sandbox"
    name: "Developer Sandbox"
    tier: "sandbox"
    api_keys:
      - "sk-gw-sandbox-1"
    allowed_routes:
      - "*"
`

	cfg, err := ParseConfig(yamlData)
	if err != nil {
		t.Fatalf("unexpected error parsing valid config: %v", err)
	}

	if cfg.Server.Host != "127.0.0.1" || cfg.Server.Port != 8080 {
		t.Errorf("unexpected server host/port: %s:%d", cfg.Server.Host, cfg.Server.Port)
	}

	upstream, found := cfg.GetUpstream("openai-main")
	if !found || upstream.EndpointURL != "https://api.openai.com" {
		t.Errorf("failed to retrieve upstream openai-main")
	}

	route, found := cfg.GetRoute("gpt-4o")
	if !found || route.ModelName != "gpt-4o" {
		t.Errorf("failed to retrieve route gpt-4o")
	}

	routes := cfg.ListRoutes()
	if len(routes) != 2 {
		t.Errorf("expected 2 routes, got %d", len(routes))
	}
}

func TestConfigValidationErrors(t *testing.T) {
	baseYAML := `
server:
  port: 8080
upstreams:
  - id: "up-1"
    endpoint_url: "https://api.openai.com"
routes:
  - alias: "route-1"
    primary_upstream: "up-1"
    model_name: "m-1"
tenants:
  - id: "t-1"
    api_keys: ["key-1"]
    allowed_routes: ["route-1"]
`

	tests := []struct {
		name          string
		mutate        func(string) string
		expectedError string
	}{
		{
			name: "invalid port zero",
			mutate: func(s string) string {
				return strings.Replace(s, "port: 8080", "port: 99999", 1)
			},
			expectedError: "server port must be between 1 and 65535",
		},
		{
			name: "no upstreams",
			mutate: func(s string) string {
				return `
server:
  port: 8080
routes:
  - alias: "route-1"
    primary_upstream: "up-1"
    model_name: "m-1"
tenants:
  - id: "t-1"
    api_keys: ["key-1"]
    allowed_routes: ["route-1"]
`
			},
			expectedError: "at least one upstream must be defined",
		},
		{
			name: "invalid upstream URL",
			mutate: func(s string) string {
				return strings.Replace(s, "https://api.openai.com", "not-a-valid-url", 1)
			},
			expectedError: "must be valid http or https URL",
		},
		{
			name: "duplicate upstream ID",
			mutate: func(s string) string {
				return `
server:
  port: 8080
upstreams:
  - id: "up-1"
    endpoint_url: "https://api.openai.com"
  - id: "up-1"
    endpoint_url: "https://api.openai.com"
routes:
  - alias: "route-1"
    primary_upstream: "up-1"
    model_name: "m-1"
tenants:
  - id: "t-1"
    api_keys: ["key-1"]
    allowed_routes: ["route-1"]
`
			},
			expectedError: "duplicate upstream id",
		},
		{
			name: "route referencing missing upstream",
			mutate: func(s string) string {
				return strings.Replace(s, "primary_upstream: \"up-1\"", "primary_upstream: \"non-existent-up\"", 1)
			},
			expectedError: "references unknown primary_upstream",
		},
		{
			name: "duplicate route alias",
			mutate: func(s string) string {
				return `
server:
  port: 8080
upstreams:
  - id: "up-1"
    endpoint_url: "https://api.openai.com"
routes:
  - alias: "route-1"
    primary_upstream: "up-1"
    model_name: "m-1"
  - alias: "route-1"
    primary_upstream: "up-1"
    model_name: "m-2"
tenants:
  - id: "t-1"
    api_keys: ["key-1"]
    allowed_routes: ["route-1"]
`
			},
			expectedError: "duplicate route alias",
		},
		{
			name: "duplicate API key across tenants",
			mutate: func(s string) string {
				return `
server:
  port: 8080
upstreams:
  - id: "up-1"
    endpoint_url: "https://api.openai.com"
routes:
  - alias: "route-1"
    primary_upstream: "up-1"
    model_name: "m-1"
tenants:
  - id: "t-1"
    api_keys: ["key-shared"]
    allowed_routes: ["route-1"]
  - id: "t-2"
    api_keys: ["key-shared"]
    allowed_routes: ["route-1"]
`
			},
			expectedError: "duplicate api_key found",
		},
		{
			name: "tenant references unconfigured allowed route",
			mutate: func(s string) string {
				return strings.Replace(s, `allowed_routes: ["route-1"]`, `allowed_routes: ["route-does-not-exist"]`, 1)
			},
			expectedError: "references unconfigured allowed_route",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rawYAML := tc.mutate(baseYAML)
			_, err := ParseConfig(rawYAML)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.expectedError)
			}
			if !strings.Contains(err.Error(), tc.expectedError) {
				t.Fatalf("expected error containing %q, got %q", tc.expectedError, err.Error())
			}
		})
	}
}
