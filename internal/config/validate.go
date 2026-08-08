package config

import (
	"fmt"
	"net/url"
	"strings"
)

// Known provider types and their adapters.
var knownTypes = map[string]bool{
	"openai":    true,
	"anthropic": true,
	"gemini":    true,
}

var knownRateLimitBackends = map[string]bool{"memory": true, "redis": true}
var knownStrategies = map[string]bool{"round-robin": true, "weighted": true, "least-latency": true}
var knownTPMEstimations = map[string]bool{"char_estimate": true, "tokenizer": true}
var knownLogLevels = map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
var knownLogFormats = map[string]bool{"json": true, "text": true}
var knownGuardTypes = map[string]bool{"llm": true, "webhook": true, "provider": true}

// Validate checks the whole configuration for correctness. It returns the
// first problem found, with a message that names the exact YAML path.
func Validate(cfg *Config) error {
	instanceNames, typeSet, modelsByType := providerInventory(cfg)

	if err := validateServer(cfg); err != nil {
		return err
	}
	if err := validateProviders(cfg, typeSet); err != nil {
		return err
	}
	if err := validateModelMapping(cfg, typeSet, modelsByType); err != nil {
		return err
	}
	if err := validateRouting(cfg, typeSet); err != nil {
		return err
	}
	usesRedis := false
	if err := validateRateLimit(cfg); err != nil {
		return err
	}
	if cfg.RateLimiting.Enabled && cfg.RateLimiting.Backend == "redis" {
		usesRedis = true
	}
	if cacheUsesRedis(cfg) {
		usesRedis = true
	}
	if err := validateRedis(cfg, usesRedis); err != nil {
		return err
	}
	if err := validateCache(cfg, instanceNames); err != nil {
		return err
	}
	if err := validateVirtualKeys(cfg); err != nil {
		return err
	}
	if err := validateGuardrails(cfg, instanceNames); err != nil {
		return err
	}
	if err := validateObservability(cfg); err != nil {
		return err
	}
	return nil
}

// providerInventory collects the names, types and served models of all
// configured provider instances for cross-reference checks.
func providerInventory(cfg *Config) (instanceNames map[string]bool, typeSet map[string]bool, modelsByType map[string]map[string]bool) {
	instanceNames = make(map[string]bool)
	typeSet = make(map[string]bool)
	modelsByType = make(map[string]map[string]bool)
	for _, p := range cfg.Providers {
		instanceNames[p.Name] = true
		typeSet[p.Type] = true
		if modelsByType[p.Type] == nil {
			modelsByType[p.Type] = make(map[string]bool)
		}
		for _, m := range p.Models {
			modelsByType[p.Type][m] = true
		}
	}
	return instanceNames, typeSet, modelsByType
}

func validateServer(cfg *Config) error {
	if cfg.Server.Port < 1 || cfg.Server.Port > 65535 {
		return fmt.Errorf("server.port must be between 1 and 65535, got %d", cfg.Server.Port)
	}
	if cfg.Server.WriteTimeout != 0 {
		// Go's WriteTimeout covers the ENTIRE response lifetime. LLM streams can
		// last minutes; a non-zero value would sever active streams (§7).
		return fmt.Errorf("server.write_timeout must be 0 (disabled); streaming responses need an unbounded write timeout — use per-provider upstream timeouts instead")
	}
	if cfg.Server.ReadTimeout < 0 {
		return fmt.Errorf("server.read_timeout must be >= 0")
	}
	return nil
}

func validateProviders(cfg *Config, typeSet map[string]bool) error {
	if len(cfg.Providers) == 0 {
		return fmt.Errorf("providers: at least one provider must be configured")
	}
	seen := make(map[string]bool)
	for _, p := range cfg.Providers {
		if p.Name == "" {
			return fmt.Errorf("providers: every provider needs a unique name")
		}
		if seen[p.Name] {
			return fmt.Errorf("providers: duplicate provider name %q", p.Name)
		}
		seen[p.Name] = true
		if !knownTypes[p.Type] {
			return fmt.Errorf("providers[%s].type: unknown provider type %q (must be openai, anthropic or gemini)", p.Name, p.Type)
		}
		if !p.APIKey.IsResolved() {
			return fmt.Errorf("providers[%s].api_key: environment variable(s) not set: %s — refusing to start with an empty key", p.Name, strings.Join(p.APIKey.UnresolvedVars(), ", "))
		}
		u, err := url.Parse(p.BaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("providers[%s].base_url: must be a valid http(s) URL, got %q", p.Name, p.BaseURL)
		}
		if len(p.Models) == 0 {
			return fmt.Errorf("providers[%s].models: at least one model is required", p.Name)
		}
		if p.Weight <= 0 {
			return fmt.Errorf("providers[%s].weight: must be > 0, got %d", p.Name, p.Weight)
		}
		if p.MaxRetries < 0 {
			return fmt.Errorf("providers[%s].max_retries: must be >= 0", p.Name)
		}
		if p.Timeout <= 0 {
			return fmt.Errorf("providers[%s].timeout: must be > 0", p.Name)
		}
		if p.Type == "anthropic" && p.DefaultMaxTokens == nil {
			return fmt.Errorf("providers[%s]: default_max_tokens is REQUIRED for type %q (Anthropic's API requires max_tokens in every request)", p.Name, p.Type)
		}
		if p.Type != "anthropic" && p.DefaultMaxTokens != nil {
			return fmt.Errorf("providers[%s]: default_max_tokens must NOT be set for type %q — injecting a cap would silently truncate responses", p.Name, p.Type)
		}
	}
	return nil
}

func validateModelMapping(cfg *Config, typeSet map[string]bool, modelsByType map[string]map[string]bool) error {
	for model, targets := range cfg.ModelMapping {
		for targetType, mappedModel := range targets {
			if mappedModel == "" {
				return fmt.Errorf("model_mapping.%s.%s: mapped model must not be empty", model, targetType)
			}
			if !knownTypes[targetType] {
				return fmt.Errorf("model_mapping.%s.%s: %q is not a provider type", model, targetType, targetType)
			}
			if !typeSet[targetType] {
				return fmt.Errorf("model_mapping.%s.%s: no provider of type %q is configured", model, targetType, targetType)
			}
			if !modelsByType[targetType][mappedModel] {
				return fmt.Errorf("model_mapping.%s.%s: model %q is not served by any %q instance", model, targetType, mappedModel, targetType)
			}
		}
	}
	return nil
}

func validateRouting(cfg *Config, typeSet map[string]bool) error {
	if cfg.Routing.Strategy != "" && !knownStrategies[cfg.Routing.Strategy] {
		return fmt.Errorf("routing.strategy: must be one of round-robin, weighted, least-latency, got %q", cfg.Routing.Strategy)
	}
	if len(cfg.Routing.FallbackOrder) == 0 {
		return fmt.Errorf("routing.fallback_order: at least one provider type is required")
	}
	for _, t := range cfg.Routing.FallbackOrder {
		if !knownTypes[t] {
			return fmt.Errorf("routing.fallback_order: %q is not a provider type", t)
		}
		if !typeSet[t] {
			return fmt.Errorf("routing.fallback_order: %q is not configured in providers", t)
		}
	}
	if cfg.Routing.CircuitBreaker.FailureThreshold <= 0 {
		return fmt.Errorf("routing.circuit_breaker.failure_threshold: must be > 0")
	}
	if cfg.Routing.CircuitBreaker.RecoveryTimeout <= 0 {
		return fmt.Errorf("routing.circuit_breaker.recovery_timeout: must be > 0")
	}
	if cfg.Routing.CircuitBreaker.HalfOpenMaxRequests <= 0 {
		return fmt.Errorf("routing.circuit_breaker.half_open_max_requests: must be > 0")
	}
	return nil
}

// validateRateLimit validates the rate_limiting block.
func validateRateLimit(cfg *Config) error {
	if !cfg.RateLimiting.Enabled {
		return nil
	}
	if !knownRateLimitBackends[cfg.RateLimiting.Backend] {
		return fmt.Errorf("rate_limiting.backend: must be memory or redis, got %q", cfg.RateLimiting.Backend)
	}
	if cfg.RateLimiting.TPMEstimation != "" && !knownTPMEstimations[cfg.RateLimiting.TPMEstimation] {
		return fmt.Errorf("rate_limiting.tpm_estimation: must be char_estimate or tokenizer, got %q", cfg.RateLimiting.TPMEstimation)
	}
	if cfg.RateLimiting.Global.RPM <= 0 || cfg.RateLimiting.Global.TPM <= 0 {
		return fmt.Errorf("rate_limiting.global: rpm and tpm must be > 0")
	}
	if cfg.RateLimiting.PerKey.RPM <= 0 || cfg.RateLimiting.PerKey.TPM <= 0 {
		return fmt.Errorf("rate_limiting.per_key: rpm and tpm must be > 0")
	}
	return nil
}

// cacheUsesRedis reports whether the cache backend is redis.
func cacheUsesRedis(cfg *Config) bool {
	return cfg.Cache.Enabled && cfg.Cache.Backend == "redis"
}

func validateRedis(cfg *Config, used bool) error {
	if !used {
		return nil
	}
	if cfg.Redis.Address == "" {
		return fmt.Errorf("redis.address: required when a backend uses redis")
	}
	if cfg.Redis.PoolSize <= 0 {
		return fmt.Errorf("redis.pool_size: must be > 0")
	}
	if !cfg.Redis.Password.IsResolved() {
		// A redis-backed feature is enabled, so a missing REDIS_PASSWORD is fatal (§7).
		return fmt.Errorf("redis.password: environment variable(s) not set: %s", strings.Join(cfg.Redis.Password.UnresolvedVars(), ", "))
	}
	return nil
}

func validateCache(cfg *Config, instanceNames map[string]bool) error {
	if !cfg.Cache.Enabled {
		return nil
	}
	if !knownRateLimitBackends[cfg.Cache.Backend] {
		return fmt.Errorf("cache.backend: must be memory or redis, got %q", cfg.Cache.Backend)
	}
	if cfg.Cache.ExactMatch.TTL <= 0 {
		return fmt.Errorf("cache.exact_match.ttl: must be > 0")
	}
	if cfg.Cache.ExactMatch.MaxEntries <= 0 {
		return fmt.Errorf("cache.exact_match.max_entries: must be > 0")
	}
	if cfg.Cache.Semantic.Enabled {
		if cfg.Cache.Semantic.SimilarityThreshold < 0 || cfg.Cache.Semantic.SimilarityThreshold > 1 {
			return fmt.Errorf("cache.semantic.similarity_threshold: must be between 0.0 and 1.0")
		}
		if !instanceNames[cfg.Cache.Semantic.EmbeddingProvider] {
			return fmt.Errorf("cache.semantic.embedding_provider: %q is not a configured provider instance", cfg.Cache.Semantic.EmbeddingProvider)
		}
		if cfg.Cache.Semantic.EmbeddingModel == "" {
			return fmt.Errorf("cache.semantic.embedding_model: must not be empty")
		}
		if cfg.Cache.Semantic.TTL <= 0 {
			return fmt.Errorf("cache.semantic.ttl: must be > 0")
		}
	}
	return nil
}

func validateVirtualKeys(cfg *Config) error {
	if !cfg.VirtualKeys.Enabled {
		return nil
	}
	seen := make(map[string]bool)
	for _, k := range cfg.VirtualKeys.Keys {
		if k.Key == "" {
			return fmt.Errorf("virtual_keys.keys: every key needs a non-empty key value")
		}
		if seen[k.Key] {
			return fmt.Errorf("virtual_keys.keys: duplicate key %q", k.Key)
		}
		seen[k.Key] = true
		if len(k.AllowedModels) == 0 {
			return fmt.Errorf("virtual_keys.keys[%s].allowed_models: at least one model (or \"*\") is required", k.Key)
		}
		if k.RateLimit.RPM <= 0 || k.RateLimit.TPM <= 0 {
			return fmt.Errorf("virtual_keys.keys[%s].rate_limit: rpm and tpm must be > 0", k.Key)
		}
	}
	return nil
}

func validateGuardrails(cfg *Config, instanceNames map[string]bool) error {
	validate := func(list []GuardConfig, section string) error {
		for _, g := range list {
			if !knownGuardTypes[g.Type] {
				return fmt.Errorf("guardrails.%s: unknown guard type %q (must be llm, webhook or provider)", section, g.Type)
			}
			switch g.Type {
			case "llm", "provider":
				if !instanceNames[g.Provider] {
					return fmt.Errorf("guardrails.%s: provider %q is not a configured provider instance", section, g.Provider)
				}
			case "webhook":
				u, err := url.Parse(g.URL)
				if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
					return fmt.Errorf("guardrails.%s: webhook url must be a valid http(s) URL", section)
				}
				if g.Timeout <= 0 {
					return fmt.Errorf("guardrails.%s: webhook timeout must be > 0", section)
				}
			}
			if g.Type == "llm" {
				if g.Model == "" {
					return fmt.Errorf("guardrails.%s: llm guard needs a model", section)
				}
				if g.Threshold < 0 || g.Threshold > 1 {
					return fmt.Errorf("guardrails.%s: threshold must be between 0.0 and 1.0", section)
				}
			}
		}
		return nil
	}
	if err := validate(cfg.Guardrails.PreRequest, "pre_request"); err != nil {
		return err
	}
	if err := validate(cfg.Guardrails.PostResponse, "post_response"); err != nil {
		return err
	}
	return nil
}

func validateObservability(cfg *Config) error {
	if cfg.Logging.Level != "" && !knownLogLevels[cfg.Logging.Level] {
		return fmt.Errorf("logging.level: must be debug, info, warn or error, got %q", cfg.Logging.Level)
	}
	if cfg.Logging.Format != "" && !knownLogFormats[cfg.Logging.Format] {
		return fmt.Errorf("logging.format: must be json or text, got %q", cfg.Logging.Format)
	}
	if cfg.Metrics.Enabled {
		if !strings.HasPrefix(cfg.Metrics.Path, "/") {
			return fmt.Errorf("metrics.path: must start with \"/\", got %q", cfg.Metrics.Path)
		}
	}
	return nil
}
