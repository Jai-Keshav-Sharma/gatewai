// Package router implements provider selection and dispatch: it resolves a
// requested model to candidate provider instances, filters out instances
// whose circuit breaker is open, picks one via the configured load-balancing
// strategy, and executes the request with the retry policy and failover
// sequence (§4.1).
package router

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/Jai-Keshav-Sharma/gatewai/internal/config"
	"github.com/Jai-Keshav-Sharma/gatewai/internal/provider"
	"github.com/Jai-Keshav-Sharma/gatewai/internal/schema"
)

// Endpoint represents a single provider INSTANCE + model combination that can
// serve a request (plan §5.6 — exact definition).
type Endpoint struct {
	ProviderName string // provider INSTANCE name from config (e.g., "openai-1"), NOT the type
	APIKey       string // the actual provider API key (resolved from virtual key or config)
	Model        string // model to request from this endpoint (after model_mapping translation)
}

// Strategy selects an endpoint from a list of candidates.
// Three implementations: RoundRobin, Weighted, LeastLatency (§5.6).
// Implementations must be safe for concurrent use — Select runs on the hot
// path from many goroutines.
type Strategy interface {
	// Name returns the strategy identifier (e.g., "round-robin", "weighted", "least-latency").
	Name() string

	// Select picks the best endpoint from the candidates.
	Select(ctx context.Context, candidates []Endpoint) (*Endpoint, error)
}

// LatencyTracker records and reports per-endpoint latency using
// Exponentially Weighted Moving Average (EWMA).
// The router calls Record() after every completed upstream request.
// The LeastLatency strategy calls Get() during selection (§5.6).
type LatencyTracker interface {
	// Record stores a latency observation for the given endpoint.
	// endpointName is the provider INSTANCE name (e.g., "openai-1").
	Record(endpointName string, latency time.Duration)

	// Get returns the current EWMA latency for the given endpoint instance.
	// Returns 0 if no observations have been recorded.
	Get(endpointName string) time.Duration
}

// Result is what a successful dispatch returns: the upstream response plus
// the instance that served it (the proxy needs the instance to translate
// the response) and the model that was actually requested upstream (after
// model_mapping translation).
type Result struct {
	Resp     *http.Response
	Instance *provider.Instance
	Model    string
}

// Router is the dispatch engine. It is stateless across requests (all state
// lives in the per-instance circuit breakers and the latency tracker), so it
// is safe for concurrent use.
type Router struct {
	registry      *provider.Registry
	client        *http.Client
	strategy      Strategy
	fallbackOrder []string
	mapping       ModelMapping
	tracker       LatencyTracker

	breakerCfg config.CircuitBreakerConfig
	breakers   map[string]*CircuitBreaker
	breakersMu sync.Mutex
}

// New builds the router from configuration. The strategy is selected by
// config ("round-robin" | "weighted" | "least-latency"); weights and the
// circuit breaker parameters also come from config — nothing is hardcoded.
func New(cfg *config.Config, reg *provider.Registry, transport *http.Transport) *Router {
	tracker := NewLatencyTracker()
	weights := make(map[string]int, len(cfg.Providers))
	for _, p := range cfg.Providers {
		weights[p.Name] = p.Weight
	}
	return &Router{
		registry:      reg,
		client:        &http.Client{Transport: transport},
		strategy:      newStrategy(cfg.Routing.Strategy, weights, tracker),
		fallbackOrder: cfg.Routing.FallbackOrder,
		mapping:       ModelMapping(cfg.ModelMapping),
		tracker:       tracker,
		breakerCfg:    cfg.Routing.CircuitBreaker,
		breakers:      make(map[string]*CircuitBreaker),
	}
}

// Dispatch executes one request through the routing pipeline (§4.1) and
// returns the first successful response. The caller owns resp.Body.
//
// The retry/failover loop lives in failover.go; this function drives the
// FAILOVER SEQUENCE: same-type instances first, then each next type in
// routing.fallback_order with the model translated via model_mapping.
// If every type is exhausted, a 502 provider_error is returned.
func (r *Router) Dispatch(ctx context.Context, req *schema.UnifiedRequest) (*Result, error) {
	home := r.homeType(req.Model)
	if home == "" {
		return nil, schema.NewInvalidRequestError(fmt.Sprintf("model %q is not configured on any provider", req.Model))
	}

	for _, t := range r.typeOrder(home) {
		// Cross-type failover translates the model (§4.1): e.g. gpt-4o →
		// claude-sonnet-4-20250514 for type "anthropic". For the home type
		// the mapping is absent, so the model stays unchanged.
		model := r.mapping.MappedModel(req.Model, t)
		candidates := r.candidates(t, model)
		if len(candidates) == 0 {
			continue // no healthy instance of this type serves the model
		}
		res, err := r.dispatchType(ctx, req, model, candidates)
		if err != nil {
			return nil, err
		}
		if res != nil {
			return res, nil
		}
		// nil result: every instance of this type exhausted retryably —
		// move to the next type in fallback_order.
	}
	return nil, schema.NewProviderError("all providers failed after retries and failover")
}

// candidates returns the healthy endpoints of the given provider type
// serving the given model. Instances whose circuit breaker is OPEN are
// filtered out (§4.1: "Filter OUT instances whose circuit breaker is OPEN").
func (r *Router) candidates(providerType, model string) []Endpoint {
	var out []Endpoint
	for _, inst := range r.registry.ByModel(model) {
		if inst.Type() != providerType {
			continue
		}
		if !r.breakerFor(inst).Allow() {
			continue
		}
		out = append(out, Endpoint{ProviderName: inst.Name(), APIKey: inst.APIKey(), Model: model})
	}
	return out
}

// typeOrder builds the failover order: the model's HOME type first (the
// same-type retry phase), then every remaining type in routing.fallback_order
// order (§4.1, FAILOVER SEQUENCE steps 1-2).
func (r *Router) typeOrder(home string) []string {
	order := make([]string, 0, len(r.fallbackOrder)+1)
	order = append(order, home)
	for _, t := range r.fallbackOrder {
		if t != home {
			order = append(order, t)
		}
	}
	return order
}

// homeType returns the type of the first instance that serves the model
// natively ("" if none does).
func (r *Router) homeType(model string) string {
	insts := r.registry.ByModel(model)
	if len(insts) == 0 {
		return ""
	}
	return insts[0].Type()
}

// breakerFor returns the per-instance circuit breaker, creating it on first
// use with the configured parameters.
func (r *Router) breakerFor(inst *provider.Instance) *CircuitBreaker {
	r.breakersMu.Lock()
	defer r.breakersMu.Unlock()
	b, ok := r.breakers[inst.Name()]
	if !ok {
		b = NewCircuitBreaker(r.breakerCfg)
		r.breakers[inst.Name()] = b
	}
	return b
}
