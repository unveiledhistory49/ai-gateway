package config

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"sort"
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
	Policies  []PolicyConfig   `yaml:"policies,omitempty"`

	// Fast lookup indices populated after validation
	upstreamsByID map[string]*UpstreamConfig
	routesByAlias map[string]*RouteConfig
	policiesByID  map[string]*PolicyConfig
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

// RouteConfig maps an incoming model alias to prioritized backend upstreams and model names.
type RouteConfig struct {
	Alias           string      `yaml:"alias" json:"alias"`
	Description     string      `yaml:"description,omitempty" json:"description,omitempty"`
	PrimaryUpstream string      `yaml:"primary_upstream,omitempty" json:"primary_upstream,omitempty"` // Backward compatibility
	ModelName       string      `yaml:"model_name,omitempty" json:"model_name,omitempty"`             // Backward compatibility
	Tiers           []RouteTier `yaml:"tiers,omitempty" json:"tiers,omitempty"`
}

// RouteTier defines a priority tier in the fallback cascade.
// Priority 0 = Primary, 1 = Fallback, 2 = Emergency.
type RouteTier struct {
	Priority int                 `yaml:"priority" json:"priority"`
	Strategy string              `yaml:"strategy,omitempty" json:"strategy,omitempty"`
	Targets  []RouteTargetConfig `yaml:"targets" json:"targets"`
}

// RouteTargetConfig defines an upstream target within a priority tier.
type RouteTargetConfig struct {
	UpstreamID string `yaml:"upstream_id" json:"upstream_id"`
	Model      string `yaml:"model" json:"model"`
	Weight     int    `yaml:"weight,omitempty" json:"weight,omitempty"`
}

// PolicyConfig defines a DLP policy with action and pattern/rule definitions.
type PolicyConfig struct {
	ID          string   `yaml:"id" json:"id"`
	Name        string   `yaml:"name,omitempty" json:"name,omitempty"`
	Description string   `yaml:"description,omitempty" json:"description,omitempty"`
	Action      string   `yaml:"action" json:"action"` // "BLOCK" or "MASK" / "REDACT"
	Patterns    []string `yaml:"patterns,omitempty" json:"patterns,omitempty"`
	Rules       []string `yaml:"rules,omitempty" json:"rules,omitempty"`
}

// AllPatterns returns the consolidated list of patterns and rules for the policy.
func (p *PolicyConfig) AllPatterns() []string {
	var pats []string
	pats = append(pats, p.Patterns...)
	pats = append(pats, p.Rules...)
	return pats
}

// TenantConfig defines a tenant identity, allowed routes, API keys, and rate limits.
type TenantConfig struct {
	ID             string            `yaml:"id"`
	Name           string            `yaml:"name"`
	Tier           string            `yaml:"tier"`
	APIKeys        []string          `yaml:"api_keys"`
	AllowedRoutes  []string          `yaml:"allowed_routes"`
	RateLimits     model.RateLimits  `yaml:"rate_limits"`
	PolicyBindings []string          `yaml:"policy_bindings,omitempty"`
	Metadata       map[string]string `yaml:"metadata"`
}

// ToTenantContext converts TenantConfig to model.TenantContext.
func (t *TenantConfig) ToTenantContext() *model.TenantContext {
	return &model.TenantContext{
		ID:             t.ID,
		Name:           t.Name,
		Tier:           t.Tier,
		AllowedRoutes:  append([]string(nil), t.AllowedRoutes...),
		RateLimits:     t.RateLimits,
		PolicyBindings: append([]string(nil), t.PolicyBindings...),
		Metadata:       t.Metadata,
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
		// Backward compatibility: If no tiers defined, synthesize Tier 0 from primary_upstream
		if len(r.Tiers) == 0 {
			if strings.TrimSpace(r.PrimaryUpstream) == "" {
				return fmt.Errorf("route '%s' has empty primary_upstream", r.Alias)
			}
			if _, exists := c.upstreamsByID[r.PrimaryUpstream]; !exists {
				return fmt.Errorf("route '%s' references unknown primary_upstream '%s'", r.Alias, r.PrimaryUpstream)
			}
			if strings.TrimSpace(r.ModelName) == "" {
				return fmt.Errorf("route '%s' has empty model_name", r.Alias)
			}
			r.Tiers = []RouteTier{
				{
					Priority: 0,
					Strategy: "priority",
					Targets: []RouteTargetConfig{
						{
							UpstreamID: r.PrimaryUpstream,
							Model:      r.ModelName,
							Weight:     100,
						},
					},
				},
			}
		} else {
			// Validate tiers
			for _, tier := range r.Tiers {
				if len(tier.Targets) == 0 {
					return fmt.Errorf("route '%s' tier %d has no targets", r.Alias, tier.Priority)
				}
				for targetIdx, target := range tier.Targets {
					if strings.TrimSpace(target.UpstreamID) == "" {
						return fmt.Errorf("route '%s' tier %d target %d has empty upstream_id", r.Alias, tier.Priority, targetIdx)
					}
					if _, exists := c.upstreamsByID[target.UpstreamID]; !exists {
						return fmt.Errorf("route '%s' references unknown upstream '%s'", r.Alias, target.UpstreamID)
					}
					if strings.TrimSpace(target.Model) == "" {
						return fmt.Errorf("route '%s' tier %d target '%s' has empty model", r.Alias, tier.Priority, target.UpstreamID)
					}
				}
			}

			// Sort tiers by priority ascending (0 = Primary, 1 = Fallback, etc.)
			sort.SliceStable(r.Tiers, func(ti, tj int) bool {
				return r.Tiers[ti].Priority < r.Tiers[tj].Priority
			})

			// Populate primary_upstream and model_name for backward compatibility
			if r.PrimaryUpstream == "" && len(r.Tiers) > 0 && len(r.Tiers[0].Targets) > 0 {
				r.PrimaryUpstream = r.Tiers[0].Targets[0].UpstreamID
				r.ModelName = r.Tiers[0].Targets[0].Model
			}
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

		// Validate policy bindings
		for _, pb := range t.PolicyBindings {
			trimmed := strings.TrimSpace(pb)
			if trimmed == "" {
				return fmt.Errorf("tenant '%s' contains empty policy_binding", t.ID)
			}
		}
	}

	// Policies validation
	c.policiesByID = make(map[string]*PolicyConfig, len(c.Policies))
	for i := range c.Policies {
		p := &c.Policies[i]
		if strings.TrimSpace(p.ID) == "" {
			return fmt.Errorf("policy index %d has empty id", i)
		}
		if _, exists := c.policiesByID[p.ID]; exists {
			return fmt.Errorf("duplicate policy id: %s", p.ID)
		}
		action := strings.ToUpper(strings.TrimSpace(p.Action))
		if action != "BLOCK" && action != "MASK" && action != "REDACT" {
			return fmt.Errorf("policy '%s' has invalid action '%s': must be BLOCK, MASK, or REDACT", p.ID, p.Action)
		}
		if action == "REDACT" {
			action = "MASK"
		}
		p.Action = action

		if len(p.AllPatterns()) == 0 {
			return fmt.Errorf("policy '%s' has no patterns or rules configured", p.ID)
		}

		c.policiesByID[p.ID] = p
	}

	// Verify tenant policy bindings after policiesByID is fully built
	for _, t := range c.Tenants {
		for _, pb := range t.PolicyBindings {
			trimmed := strings.TrimSpace(pb)
			if _, exists := c.policiesByID[trimmed]; !exists {
				return fmt.Errorf("tenant '%s' references unconfigured policy '%s'", t.ID, trimmed)
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

// GetPolicy retrieves PolicyConfig by ID.
func (c *Config) GetPolicy(id string) (*PolicyConfig, bool) {
	p, ok := c.policiesByID[id]
	return p, ok
}

// ListPolicies returns all configured PolicyConfig items.
func (c *Config) ListPolicies() []PolicyConfig {
	policies := make([]PolicyConfig, len(c.Policies))
	copy(policies, c.Policies)
	return policies
}
