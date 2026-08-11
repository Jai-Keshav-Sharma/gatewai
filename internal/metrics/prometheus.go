// Package metrics defines and records Prometheus metrics (§4.1 step 10).
//
// LABEL CARDINALITY RULES (prevent Prometheus explosion, §9):
//
//	SAFE labels: provider (INSTANCE name), model, status_code, method, cache_hit
//	NEVER use as labels: virtual_key, request_id, user content
//	Per-key metrics use a separate counter with a HASHED key dimension.
//
// Why the rules matter: every unique label combination creates a new time
// series in Prometheus. Using request_id or user content as labels would
// create one series per request — unbounded memory, a classic incident.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// RequestsTotal counts every chat completion request.
	// cache_hit is "true"/"false" (a bool label would create two series anyway,
	// but string keeps the label type explicit).
	RequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_requests_total",
		Help: "Total chat completion requests.",
	}, []string{"provider", "model", "status_code", "cache_hit"})

	// RequestDuration records request latency. A histogram (not a gauge) is
	// the right type: it captures the DISTRIBUTION, so P50/P95/P99 can be
	// derived without storing every observation.
	RequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gatewai_request_duration_seconds",
		Help:    "Chat completion request duration in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"provider", "model", "cache_hit"})

	// TokensTotal counts input and output tokens (kind: "input"/"output").
	TokensTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_tokens_total",
		Help: "Tokens consumed, by direction.",
	}, []string{"provider", "model", "kind"})

	// CostUSDTotal tracks spend: tokens × model pricing (§6 Model struct).
	CostUSDTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_cost_usd_total",
		Help: "Estimated USD cost of tokens consumed.",
	}, []string{"provider", "model"})

	// KeyRequestsTotal is the per-key counter with a HASHED key dimension —
	// the raw virtual key must never appear in a label.
	KeyRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_key_requests_total",
		Help: "Requests per virtual key (dimension is the SHA-256 of the key).",
	}, []string{"key_hash"})
)

// Pricer resolves model pricing (USD per 1M tokens) for cost accounting.
// Built at wiring time from the provider registries' catalogs.
type Pricer func(provider, model string) (inputPer1M, outputPer1M float64, ok bool)
