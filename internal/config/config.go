// Package config loads, validates and normalizes the gatewai.yaml configuration.
//
// The structs below mirror the exact YAML schema from the implementation plan
// (§7). Every field listed in the plan is supported; validation lives in
// validate.go.
package config

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"time"

	"gopkg.in/yaml.v3"
)

// Load reads the config file at path, decodes it strictly (unknown fields are
// an error — a typo'd key silently being ignored is how misconfiguration
// hides), resolves environment variable references, and validates everything.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true) // reject unknown YAML keys

	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}

	if err := Validate(&cfg); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return &cfg, nil
}

// Config is the root configuration object. Each section maps to a YAML block.
type Config struct {
	Server       ServerConfig                 `yaml:"server"`
	Providers    []ProviderConfig             `yaml:"providers"`
	ModelMapping map[string]map[string]string `yaml:"model_mapping"`
	Routing      RoutingConfig                `yaml:"routing"`
	RateLimiting RateLimitConfig              `yaml:"rate_limiting"`
	Cache        CacheConfig                  `yaml:"cache"`
	Redis        RedisConfig                  `yaml:"redis"`
	VirtualKeys  VirtualKeysConfig            `yaml:"virtual_keys"`
	Guardrails   GuardrailsConfig             `yaml:"guardrails"`
	Logging      LoggingConfig                `yaml:"logging"`
	Metrics      MetricsConfig                `yaml:"metrics"`
}

// ServerConfig is the HTTP server tuning block.
type ServerConfig struct {
	Host             string   `yaml:"host"`
	Port             int      `yaml:"port"`
	ReadTimeout      Duration `yaml:"read_timeout"`
	WriteTimeout     Duration `yaml:"write_timeout"` // MUST be 0 — see validate.go
	GracefulShutdown Duration `yaml:"graceful_shutdown"`
}

// ProviderConfig is a single provider INSTANCE (e.g. "openai-1").
type ProviderConfig struct {
	Name             string    `yaml:"name"`
	Type             string    `yaml:"type"` // "openai" | "anthropic" | "gemini"
	APIKey           EnvString `yaml:"api_key"`
	BaseURL          string    `yaml:"base_url"`
	Models           []string  `yaml:"models"`
	Weight           int       `yaml:"weight"`
	MaxRetries       int       `yaml:"max_retries"`
	Timeout          Duration  `yaml:"timeout"`
	DefaultMaxTokens *int      `yaml:"default_max_tokens"` // anthropic-only, see §7
}

// RoutingConfig is the load balancing and failover block.
type RoutingConfig struct {
	Strategy       string               `yaml:"strategy"`
	FallbackOrder  []string             `yaml:"fallback_order"`
	CircuitBreaker CircuitBreakerConfig `yaml:"circuit_breaker"`
}

// CircuitBreakerConfig tunes the circuit breaker state machine (§4.1).
type CircuitBreakerConfig struct {
	FailureThreshold    int      `yaml:"failure_threshold"`
	RecoveryTimeout     Duration `yaml:"recovery_timeout"`
	HalfOpenMaxRequests int      `yaml:"half_open_max_requests"`
}

// RateLimitConfig is the rate limiting block.
type RateLimitConfig struct {
	Enabled       bool        `yaml:"enabled"`
	Backend       string      `yaml:"backend"`        // "memory" | "redis"
	TPMEstimation string      `yaml:"tpm_estimation"` // "char_estimate" | "tokenizer"
	Global        LimitConfig `yaml:"global"`
	PerKey        LimitConfig `yaml:"per_key"`
}

// LimitConfig is an RPM/TPM pair.
type LimitConfig struct {
	RPM int `yaml:"rpm"`
	TPM int `yaml:"tpm"`
}

// CacheConfig is the response cache block.
type CacheConfig struct {
	Enabled    bool             `yaml:"enabled"`
	Backend    string           `yaml:"backend"` // "memory" | "redis"
	ExactMatch ExactMatchConfig `yaml:"exact_match"`
	Semantic   SemanticConfig   `yaml:"semantic"`
}

// ExactMatchConfig tunes the exact-match cache.
type ExactMatchConfig struct {
	TTL        Duration `yaml:"ttl"`
	MaxEntries int      `yaml:"max_entries"`
}

// SemanticConfig tunes semantic (vector) cache lookups.
type SemanticConfig struct {
	Enabled             bool     `yaml:"enabled"`
	SimilarityThreshold float64  `yaml:"similarity_threshold"`
	EmbeddingProvider   string   `yaml:"embedding_provider"` // provider INSTANCE name
	EmbeddingModel      string   `yaml:"embedding_model"`
	TTL                 Duration `yaml:"ttl"`
}

// RedisConfig is the shared Redis connection block.
type RedisConfig struct {
	Address  string    `yaml:"address"`
	Password EnvString `yaml:"password"`
	DB       int       `yaml:"db"`
	PoolSize int       `yaml:"pool_size"`
}

// VirtualKeysConfig is the virtual API key block.
type VirtualKeysConfig struct {
	Enabled bool               `yaml:"enabled"`
	Keys    []VirtualKeyConfig `yaml:"keys"`
}

// VirtualKeyConfig is a single virtual key with its permissions.
type VirtualKeyConfig struct {
	Key           string      `yaml:"key"`
	Description   string      `yaml:"description"`
	AllowedModels []string    `yaml:"allowed_models"` // "*" = all models
	RateLimit     LimitConfig `yaml:"rate_limit"`
}

// GuardrailsConfig is the content-safety block.
type GuardrailsConfig struct {
	PreRequest   []GuardConfig `yaml:"pre_request"`
	PostResponse []GuardConfig `yaml:"post_response"`
	BufferMode   bool          `yaml:"buffer_mode"`
}

// GuardConfig is a single guardrail classifier configuration.
type GuardConfig struct {
	Type      string   `yaml:"type"`     // "llm" | "webhook" | "provider"
	Provider  string   `yaml:"provider"` // provider INSTANCE name
	Model     string   `yaml:"model"`
	Prompt    string   `yaml:"prompt"`
	Threshold float64  `yaml:"threshold"`
	URL       string   `yaml:"url"` // webhook only
	Timeout   Duration `yaml:"timeout"`
}

// LoggingConfig is the logging block.
type LoggingConfig struct {
	Level  string `yaml:"level"`  // "debug" | "info" | "warn" | "error"
	Format string `yaml:"format"` // "json" | "text"
}

// MetricsConfig is the Prometheus block.
type MetricsConfig struct {
	Enabled bool   `yaml:"enabled"`
	Path    string `yaml:"path"`
}

// Duration is a time.Duration that parses from a YAML duration string like "30s".
type Duration time.Duration

// String renders the duration in Go's duration format.
func (d Duration) String() string { return time.Duration(d).String() }

// UnmarshalYAML parses a duration string (time.ParseDuration format).
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
		return fmt.Errorf("duration must be a string like %q, got %q", "30s", node.Value)
	}
	parsed, err := time.ParseDuration(node.Value)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", node.Value, err)
	}
	*d = Duration(parsed)
	return nil
}

// envVarPattern matches ${VAR_NAME} references in config values.
var envVarPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// EnvString is a config string that may reference environment variables
// via ${VAR_NAME}. References are resolved at load time.
//
// If a referenced variable is missing, the literal "${VAR_NAME}" text is kept
// in the value and IsResolved() reports false — validation then decides
// whether that is fatal (feature enabled) or fine (feature disabled).
// This implements the plan's rule: never silently use an empty string for a
// missing required variable (§7).
type EnvString string

// UnmarshalYAML resolves environment variable references in the scalar.
func (e *EnvString) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("expected a string, got %q", node.ShortTag())
	}
	value := node.Value
	if resolved, ok := interpolateEnv(value); ok {
		*e = EnvString(resolved)
	} else {
		*e = EnvString(value) // keep the literal ${VAR} for validation
	}
	return nil
}

// IsResolved reports whether every ${VAR} reference has been resolved.
func (e EnvString) IsResolved() bool {
	return !envVarPattern.MatchString(string(e))
}

// UnresolvedVars returns the names of the environment variables this string
// references but that are not set.
func (e EnvString) UnresolvedVars() []string {
	matches := envVarPattern.FindAllStringSubmatch(string(e), -1)
	names := make([]string, 0, len(matches))
	for _, m := range matches {
		names = append(names, m[1])
	}
	return names
}

// interpolateEnv replaces every ${VAR} in s with the variable's value.
// Returns ok=false (and the original string) if ANY referenced variable is
// missing — a partially interpolated value would be silently wrong.
func interpolateEnv(s string) (string, bool) {
	if !envVarPattern.MatchString(s) {
		return s, true
	}
	missing := false
	result := envVarPattern.ReplaceAllStringFunc(s, func(m string) string {
		name := envVarPattern.FindStringSubmatch(m)[1]
		if v, ok := os.LookupEnv(name); ok {
			return v
		}
		missing = true
		return m
	})
	if missing {
		return s, false
	}
	return result, true
}
