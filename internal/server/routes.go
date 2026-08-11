package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Jai-Keshav-Sharma/gatewai/internal/cache"
	"github.com/Jai-Keshav-Sharma/gatewai/internal/config"
	"github.com/Jai-Keshav-Sharma/gatewai/internal/guardrail"
	"github.com/Jai-Keshav-Sharma/gatewai/internal/metrics"
	"github.com/Jai-Keshav-Sharma/gatewai/internal/middleware"
	"github.com/Jai-Keshav-Sharma/gatewai/internal/provider"
	"github.com/Jai-Keshav-Sharma/gatewai/internal/proxy"
	"github.com/Jai-Keshav-Sharma/gatewai/internal/ratelimit"
	"github.com/Jai-Keshav-Sharma/gatewai/internal/router"
	"github.com/Jai-Keshav-Sharma/gatewai/internal/schema"
	"github.com/Jai-Keshav-Sharma/gatewai/internal/virtualkey"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
)

// NewRoutes registers all routes and composes the middleware chain in the
// exact §4.1 order: bodyparser (0) → auth (4) → rate limit (5) → cache (6) →
// proxy handler. Later phases insert request-id, logging, metrics and
// guardrails into their positions.
func NewRoutes(cfg *config.Config, reg *provider.Registry, transport *http.Transport) (http.Handler, error) {
	mux := http.NewServeMux()

	// --- Governance wiring (Phase 4) ---

	keyStore, err := virtualkey.NewStore(cfg.VirtualKeys)
	if err != nil {
		return nil, err
	}

	redisClient, err := buildRedis(cfg)
	if err != nil {
		return nil, err
	}

	limiter, limits, err := buildLimiter(cfg, keyStore, redisClient)
	if err != nil {
		return nil, err
	}

	c, err := buildCache(cfg, reg, redisClient, transport)
	if err != nil {
		return nil, err
	}

	var chat http.Handler = proxy.NewHandler(router.New(cfg, reg, transport))
	est, err := estimator(cfg)
	if err != nil {
		return nil, err
	}
	// Full §4.1 chain: 0 bodyparser → 1 requestid → 2 logger → 3 metrics →
	// 4 auth → 5 rate limit → 6 cache → 7 guardrail → proxy handler.
	guards, err := buildGuards(cfg, reg, transport)
	if err != nil {
		return nil, err
	}
	chat = middleware.Chain(chat,
		middleware.BodyParser,
		middleware.RequestID(),
		middleware.Logger(),
		middleware.Metrics(cfg.Metrics.Enabled, pricerFor(reg)),
		middleware.Auth(cfg.VirtualKeys.Enabled, keyStore),
		middleware.NewRateLimit(limiter, cfg.RateLimiting.Enabled,
			est,
			"global", "global:tpm",
			keyDim, keyTPMDim,
			limitFor(cfg, limits),
		).Middleware(),
		middleware.NewCache(c, cfg.Cache.Enabled,
			time.Duration(cfg.Cache.ExactMatch.TTL),
			cfg.Cache.Semantic.Enabled,
			cfg.Cache.Semantic.SimilarityThreshold,
			embedderFor(cfg, reg, transport),
			time.Duration(cfg.Cache.Semantic.TTL),
		).Middleware(),
		middleware.NewGuardrail(guards.pre, guards.post, cfg.Guardrails.BufferMode).Middleware(),
	)
	mux.Handle("POST /v1/chat/completions", chat)

	// GET /v1/models (§8.1): list every model across all providers, with the
	// same authentication as other /v1/* routes (§8.2).
	mux.Handle("GET /v1/models", middleware.Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var models []map[string]string
		for _, inst := range reg.Instances() {
			for _, m := range inst.Models() {
				models = append(models, map[string]string{
					"id":       m.ID,
					"object":   "model",
					"owned_by": inst.Name(),
				})
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": models})
	}), middleware.Auth(cfg.VirtualKeys.Enabled, keyStore)))

	// Prometheus scrape endpoint (§8.1) — not under /v1, so it stays open.
	if cfg.Metrics.Enabled {
		mux.Handle(cfg.Metrics.Path, promhttp.Handler())
	}

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"status":"ok"}`)
	})

	return mux, nil
}

// keyDim hashes the bearer key so key material never lands in map keys or
// Redis keys (§5.4 SECURITY rule).
func keyDim(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "key:" + hex.EncodeToString(sum[:])
}

func keyTPMDim(key string) string {
	return keyDim(key) + ":tpm"
}

// buildRedis creates the shared Redis client when any backend uses it.
func buildRedis(cfg *config.Config) (*redis.Client, error) {
	if cfg.RateLimiting.Backend != "redis" && cfg.Cache.Backend != "redis" {
		return nil, nil
	}
	return redis.NewClient(&redis.Options{
		Addr:     cfg.Redis.Address,
		Password: string(cfg.Redis.Password),
		DB:       cfg.Redis.DB,
		PoolSize: cfg.Redis.PoolSize,
	}), nil
}

// buildLimiter constructs the limiter and the dimension → limit map.
func buildLimiter(cfg *config.Config, store *virtualkey.Store, redisClient *redis.Client) (ratelimit.Limiter, map[string]int, error) {
	limits := map[string]int{
		"global":     cfg.RateLimiting.Global.RPM,
		"global:tpm": cfg.RateLimiting.Global.TPM,
	}
	// Per-key dimensions, one pair per configured virtual key.
	for _, k := range cfg.VirtualKeys.Keys {
		rpm := k.RateLimit.RPM
		if rpm <= 0 {
			rpm = cfg.RateLimiting.PerKey.RPM
		}
		tpm := k.RateLimit.TPM
		if tpm <= 0 {
			tpm = cfg.RateLimiting.PerKey.TPM
		}
		limits[keyDim(k.Key)] = rpm
		limits[keyTPMDim(k.Key)] = tpm
	}
	if cfg.RateLimiting.Backend == "redis" {
		return ratelimit.NewRedis(redisClient, limits, cfg.RateLimiting.PerKey.RPM, cfg.RateLimiting.PerKey.TPM, time.Minute), limits, nil
	}
	return ratelimit.NewMemory(limits, cfg.RateLimiting.PerKey.RPM, cfg.RateLimiting.PerKey.TPM), limits, nil
}

// estimator returns the TPM estimation strategy from config.
func estimator(cfg *config.Config) (middleware.Estimator, error) {
	if cfg.RateLimiting.TPMEstimation == "tokenizer" {
		return nil, fmt.Errorf("rate_limiting.tpm_estimation: \"tokenizer\" is not implemented yet — use \"char_estimate\"")
	}
	return middleware.CharEstimator{}, nil
}

// limitFor resolves the numeric limit for a dimension, falling back to the
// per-key defaults for dimensions that were not pre-registered (virtual keys
// disabled).
func limitFor(cfg *config.Config, limits map[string]int) func(string) int {
	return func(dimension string) int {
		if v, ok := limits[dimension]; ok {
			return v
		}
		if strings.HasSuffix(dimension, ":tpm") {
			return cfg.RateLimiting.PerKey.TPM
		}
		return cfg.RateLimiting.PerKey.RPM
	}
}

// buildCache constructs the configured cache backend and wraps it with the
// semantic store when enabled.
func buildCache(cfg *config.Config, reg *provider.Registry, redisClient *redis.Client, transport *http.Transport) (cache.Cache, error) {
	if !cfg.Cache.Enabled {
		return nil, nil
	}
	var c cache.Cache
	if cfg.Cache.Backend == "redis" {
		c = cache.NewRedis(redisClient)
	} else {
		c = cache.NewMemory(cfg.Cache.ExactMatch.MaxEntries)
	}
	if cfg.Cache.Semantic.Enabled {
		if cfg.Cache.Backend == "redis" {
			return nil, fmt.Errorf("cache.semantic with redis backend (Redis VSS) is not implemented yet — use \"memory\" or disable semantic caching")
		}
		c = cache.NewMemorySemantic(c)
	}
	return c, nil
}

// guardSet holds the built pre-request and post-response classifiers.
type guardSet struct {
	pre  []guardrail.Guard
	post []guardrail.Guard
}

// buildGuards constructs the configured classifiers (§7 guardrails block).
// Guard references point at provider INSTANCES (llm/provider types) or URLs
// (webhook type) — all validated at config load time.
func buildGuards(cfg *config.Config, reg *provider.Registry, transport *http.Transport) (guardSet, error) {
	client := &http.Client{Transport: transport}
	build := func(list []config.GuardConfig, set *[]guardrail.Guard) error {
		for _, gc := range list {
			switch gc.Type {
			case "llm":
				inst, ok := reg.Get(gc.Provider)
				if !ok {
					return fmt.Errorf("guardrail: unknown provider instance %q", gc.Provider)
				}
				*set = append(*set, guardrail.NewLLMGuard(
					"llm:"+gc.Provider+":"+gc.Model,
					inst.BaseURL(), inst.APIKey(), gc.Model, gc.Prompt, gc.Threshold,
					time.Duration(gc.Timeout), client))
			case "webhook":
				*set = append(*set, guardrail.NewWebhookGuard(
					"webhook:"+gc.URL, gc.URL, time.Duration(gc.Timeout), client))
			case "provider":
				inst, ok := reg.Get(gc.Provider)
				if !ok {
					return fmt.Errorf("guardrail: unknown provider instance %q", gc.Provider)
				}
				*set = append(*set, guardrail.NewProviderGuard(
					"provider:"+gc.Provider, inst.BaseURL(), inst.APIKey(),
					time.Duration(gc.Timeout), client))
			}
		}
		return nil
	}
	gs := guardSet{}
	if err := build(cfg.Guardrails.PreRequest, &gs.pre); err != nil {
		return gs, err
	}
	if err := build(cfg.Guardrails.PostResponse, &gs.post); err != nil {
		return gs, err
	}
	return gs, nil
}

// pricerFor builds the metric pricing resolver from the registries' catalogs:
// provider (INSTANCE name) + model → USD per 1M tokens.
func pricerFor(reg *provider.Registry) metrics.Pricer {
	prices := make(map[string]map[string]schema.Model)
	for _, inst := range reg.Instances() {
		m := make(map[string]schema.Model)
		for _, model := range inst.Models() {
			m[model.ID] = model
		}
		prices[inst.Name()] = m
	}
	return func(provider, model string) (float64, float64, bool) {
		models, ok := prices[provider]
		if !ok {
			return 0, 0, false
		}
		m, ok := models[model]
		if !ok {
			return 0, 0, false
		}
		return m.InputPricePer1M, m.OutputPricePer1M, true
	}
}

// embedderFor builds the embeddings client for semantic caching (§7: the
// embedding_provider is an INSTANCE — keys and base URL come from it).
func embedderFor(cfg *config.Config, reg *provider.Registry, transport *http.Transport) cache.Embedder {
	if !cfg.Cache.Enabled || !cfg.Cache.Semantic.Enabled {
		return nil
	}
	inst, ok := reg.Get(cfg.Cache.Semantic.EmbeddingProvider)
	if !ok {
		return nil // unreachable: config validation guarantees the instance exists
	}
	return cache.NewHTTPEmbedder(inst.BaseURL(), inst.APIKey(), cfg.Cache.Semantic.EmbeddingModel,
		&http.Client{Transport: transport})
}
