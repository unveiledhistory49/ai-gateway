package config

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/company/ai-gateway/internal/model"
	"gopkg.in/yaml.v3"
)

// Default server settings.
const (
	DefaultServerHost          = "0.0.0.0"
	DefaultServerPort          = 8080
	DefaultReadTimeoutSeconds  = 15
	DefaultWriteTimeoutSeconds = 60
	DefaultIdleTimeoutSeconds  = 120
	DefaultMaxBodyBytes        = 16 * 1024 * 1024 // 16 MB
	DefaultUpstreamTimeoutSec  = 30
)

// Config represents the top-level gateway configuration schema.
type Config struct {
	Server    ServerConfig     `yaml:"server"`
	Upstreams []UpstreamConfig `yaml:"upstreams"`
	Routes    []RouteConfig    `yaml:"routes"`
	Tenants   []TenantConfig   `yaml:"tenants"`

	// Fast lookup indices populated after validation
	upstreamsByID map[string]*UpstreamConfig
	routesByAlias map[string]*RouteConfig
}

// ServerConfig defines the ingress listener and connection timeouts.
type ServerConfig struct {
	Host                string `yaml:"host"`
	Port                int    `yaml:"port"`
	ReadTimeoutSeconds  int    `yaml:"read_timeout_seconds"`
	WriteTimeoutSeconds int    `yaml:"write_timeout_seconds"`
	IdleTimeoutSeconds  int    `yaml:"idle_timeout_seconds"`
	MaxBodyBytes        int64  `yaml:"max_body_bytes"`
}

// ReadTimeout returns the configured read timeout as a time.Duration.
func (s *ServerConfig) ReadTimeout() time.Duration {
	return time.Duration(s.ReadTimeoutSeconds) * time.Second
}

// WriteTimeout returns the configured write timeout as a time.Duration.
func (s *ServerConfig) WriteTimeout() time.Duration {
	return time.Duration(s.WriteTimeoutSeconds) * time.Second
}

// IdleTimeout returns the configured idle timeout as a time.Duration.
func (s *ServerConfig) IdleTimeout() time.Duration {
	return time.Duration(s.IdleTimeoutSeconds) * time.Second
}

// UpstreamConfig defines a backend provider endpoint and its credentials.
type UpstreamConfig struct {
	ID             string `yaml:"id"`
	Provider       string `yaml:"provider"`
	EndpointURL    string `yaml:"endpoint_url"`
	APIKey         string `yaml:"api_key"`
	TimeoutSeconds int    `yaml:"timeout_seconds"`
}

// Timeout returns the upstream timeout as a time.Duration.
func (u *UpstreamConfig) Timeout() time.Duration {
	return time.Duration(u.TimeoutSeconds) * time.Second
}

// RouteConfig maps an incoming model alias to a backend upstream and actual model name.
type RouteConfig struct {
	Alias           string `yaml:"alias"`
	PrimaryUpstream string `yaml:"primary_upstream"`
	ModelName       string `yaml:"model_name"`
}

// TenantConfig defines a tenant identity, allowed routes, API keys, and rate limits.
type TenantConfig struct {
	ID            string            `yaml:"id"`
	Name          string            `yaml:"name"`
	Tier          string            `yaml:"tier"`
	APIKeys       []string          `yaml:"api_keys"`
	AllowedRoutes []string          `yaml:"allowed_routes"`
	RateLimits    model.RateLimits  `yaml:"rate_limits"`
	Metadata      map[string]string `yaml:"metadata"`
}

// ToTenantContext converts TenantConfig to model.TenantContext.
func (t *TenantConfig) ToTenantContext() *model.TenantContext {
	return &model.TenantContext{
		ID:            t.ID,
		Name:          t.Name,
		Tier:          t.Tier,
		AllowedRoutes: append([]string(nil), t.AllowedRoutes...),
		RateLimits:    t.RateLimits,
		Metadata:      t.Metadata,
	}
}

// envVarPattern matches ${VAR_NAME} or ${VAR_NAME:-default} or ${VAR_NAME:default}.
var envVarPattern = regexp.MustCompile(`\$\{([A-Za-z0-9_]+)(?::-([^}]*)|:([^}]*))?\}`)

// ExpandEnv substitutes environment variables in the format ${VAR} or ${VAR:-default} or ${VAR:default}.
func ExpandEnv(content string) string {
	return envVarPattern.ReplaceAllStringFunc(content, func(match string) string {
		submatches := envVarPattern.FindStringSubmatch(match)
		if len(submatches) < 2 {
			return match
		}
		varName := submatches[1]
		val, exists := os.LookupEnv(varName)
		if exists && val != "" {
			return val
		}

		// Check for default value: submatches[2] is :-default, submatches[3] is :default
		defaultVal := ""
		if len(submatches) > 2 && submatches[2] != "" {
			defaultVal = submatches[2]
		} else if len(submatches) > 3 && submatches[3] != "" {
			defaultVal = submatches[3]
		}

		if exists && val == "" && defaultVal == "" {
			return ""
		}
		if !exists && defaultVal != "" {
			return defaultVal
		}
		if val != "" {
			return val
		}
		return defaultVal
	})
}

// LoadConfig reads, expands environment variables, unmarshals YAML, and validates a config file.
func LoadConfig(filePath string) (*Config, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file %s: %w", filePath, err)
	}

	return ParseConfig(string(data))
}

// ParseConfig parses and validates YAML configuration text with env substitution.
func ParseConfig(rawYAML string) (*Config, error) {
	expanded := ExpandEnv(rawYAML)

	var cfg Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse yaml configuration: %w", err)
	}

	if err := cfg.applyDefaultsAndValidate(); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}

	return &cfg, nil
}

// applyDefaultsAndValidate validates all fields and builds internal lookup tables.
func (c *Config) applyDefaultsAndValidate() error {
	// Server defaults and validation
	if c.Server.Host == "" {
		c.Server.Host = DefaultServerHost
	}
	if c.Server.Port == 0 {
		c.Server.Port = DefaultServerPort
	} else if c.Server.Port < 1 || c.Server.Port > 65535 {
		return fmt.Errorf("server port must be between 1 and 65535, got %d", c.Server.Port)
	}

	if c.Server.ReadTimeoutSeconds <= 0 {
		c.Server.ReadTimeoutSeconds = DefaultReadTimeoutSeconds
	}
	if c.Server.WriteTimeoutSeconds <= 0 {
		c.Server.WriteTimeoutSeconds = DefaultWriteTimeoutSeconds
	}
	if c.Server.IdleTimeoutSeconds <= 0 {
		c.Server.IdleTimeoutSeconds = DefaultIdleTimeoutSeconds
	}
	if c.Server.MaxBodyBytes <= 0 {
		c.Server.MaxBodyBytes = DefaultMaxBodyBytes
	}

	// Upstreams validation
	if len(c.Upstreams) == 0 {
		return fmt.Errorf("at least one upstream must be defined")
	}

	c.upstreamsByID = make(map[string]*UpstreamConfig, len(c.Upstreams))
	for i := range c.Upstreams {
		u := &c.Upstreams[i]
		if strings.TrimSpace(u.ID) == "" {
			return fmt.Errorf("upstream index %d has empty id", i)
		}
		if _, exists := c.upstreamsByID[u.ID]; exists {
			return fmt.Errorf("duplicate upstream id: %s", u.ID)
		}

		if strings.TrimSpace(u.EndpointURL) == "" {
			return fmt.Errorf("upstream '%s' has empty endpoint_url", u.ID)
		}
		parsedURL, err := url.ParseRequestURI(u.EndpointURL)
		if err != nil || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") || parsedURL.Host == "" {
			return fmt.Errorf("upstream '%s' has invalid endpoint_url '%s': must be valid http or https URL", u.ID, u.EndpointURL)
		}

		if u.TimeoutSeconds <= 0 {
			u.TimeoutSeconds = DefaultUpstreamTimeoutSec
		}

		c.upstreamsByID[u.ID] = u
	}

	// Routes validation
	if len(c.Routes) == 0 {
		return fmt.Errorf("at least one route must be defined")
	}

	c.routesByAlias = make(map[string]*RouteConfig, len(c.Routes))
	for i := range c.Routes {
		r := &c.Routes[i]
		if strings.TrimSpace(r.Alias) == "" {
			return fmt.Errorf("route index %d has empty alias", i)
		}
		if _, exists := c.routesByAlias[r.Alias]; exists {
			return fmt.Errorf("duplicate route alias: %s", r.Alias)
		}
		if strings.TrimSpace(r.PrimaryUpstream) == "" {
			return fmt.Errorf("route '%s' has empty primary_upstream", r.Alias)
		}
		if _, exists := c.upstreamsByID[r.PrimaryUpstream]; !exists {
			return fmt.Errorf("route '%s' references unknown primary_upstream '%s'", r.Alias, r.PrimaryUpstream)
		}
		if strings.TrimSpace(r.ModelName) == "" {
			return fmt.Errorf("route '%s' has empty model_name", r.Alias)
		}

		c.routesByAlias[r.Alias] = r
	}

	// Tenants validation
	if len(c.Tenants) == 0 {
		return fmt.Errorf("at least one tenant must be defined")
	}

	seenTenantIDs := make(map[string]struct{}, len(c.Tenants))
	seenAPIKeys := make(map[string]string) // key -> tenantID

	for i := range c.Tenants {
		t := &c.Tenants[i]
		if strings.TrimSpace(t.ID) == "" {
			return fmt.Errorf("tenant index %d has empty id", i)
		}
		if _, exists := seenTenantIDs[t.ID]; exists {
			return fmt.Errorf("duplicate tenant id: %s", t.ID)
		}
		seenTenantIDs[t.ID] = struct{}{}

		if len(t.APIKeys) == 0 {
			return fmt.Errorf("tenant '%s' has no api_keys configured", t.ID)
		}

		for _, k := range t.APIKeys {
			trimmedKey := strings.TrimSpace(k)
			if trimmedKey == "" {
				return fmt.Errorf("tenant '%s' contains empty api_key", t.ID)
			}
			if existingTenant, exists := seenAPIKeys[trimmedKey]; exists {
				return fmt.Errorf("duplicate api_key found in tenant '%s', already registered to '%s'", t.ID, existingTenant)
			}
			seenAPIKeys[trimmedKey] = t.ID
		}

		// Validate allowed routes
		if len(t.AllowedRoutes) == 0 {
			return fmt.Errorf("tenant '%s' has no allowed_routes configured", t.ID)
		}
		for _, route := range t.AllowedRoutes {
			if route == "*" {
				continue
			}
			if _, exists := c.routesByAlias[route]; !exists {
				return fmt.Errorf("tenant '%s' references unconfigured allowed_route '%s'", t.ID, route)
			}
		}
	}

	return nil
}

// GetUpstream retrieves UpstreamConfig by ID.
func (c *Config) GetUpstream(id string) (*UpstreamConfig, bool) {
	u, ok := c.upstreamsByID[id]
	return u, ok
}

// GetRoute retrieves RouteConfig by model alias.
func (c *Config) GetRoute(alias string) (*RouteConfig, bool) {
	r, ok := c.routesByAlias[alias]
	return r, ok
}

// ListRoutes returns all configured RouteConfig items.
func (c *Config) ListRoutes() []RouteConfig {
	routes := make([]RouteConfig, len(c.Routes))
	copy(routes, c.Routes)
	return routes
}
