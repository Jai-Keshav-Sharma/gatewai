package middleware

import (
	"fmt"
	"net/http"

	"github.com/Jai-Keshav-Sharma/gatewai/internal/ratelimit"
	"github.com/Jai-Keshav-Sharma/gatewai/internal/schema"
)

// RateLimit is middleware step 5 of the chain (§4.1):
//
//	a. RPM check: 1 request unit (pre-request)
//	b. TPM pre-check: estimate prompt tokens, reserve capacity
//	c. TPM post-charge: after the response, charge the actual tokens and
//	   adjust the reservation (runs as a deferred post-response hook).
//
// CACHE HITS ARE CHARGED TOO (non-negotiable): the proxy records the cached
// response's token counts in the RequestContext, so the post-charge hook
// runs identically for cache hits — the cache can never become a rate-limit
// bypass (§4.1 step 11).
type RateLimit struct {
	limiter  ratelimit.Limiter
	enabled  bool
	estimate Estimator
	// dimension helpers — the wiring layer computes these from config.
	globalRPM string
	globalTPM string
	keyRPM    func(key string) string
	keyTPM    func(key string) string
	// limitFor resolves the numeric limit for a dimension (for error
	// messages like "Rate limit exceeded: 100 RPM for key gw-team-frontend").
	limitFor func(dimension string) int
}

// NewRateLimit builds the middleware from a limiter and dimension helpers.
func NewRateLimit(limiter ratelimit.Limiter, enabled bool, estimator Estimator,
	globalRPM, globalTPM string, keyRPM, keyTPM func(key string) string,
	limitFor func(dimension string) int) *RateLimit {
	return &RateLimit{
		limiter:   limiter,
		enabled:   enabled,
		estimate:  estimator,
		globalRPM: globalRPM,
		globalTPM: globalTPM,
		keyRPM:    keyRPM,
		keyTPM:    keyTPM,
		limitFor:  limitFor,
	}
}

// Middleware implements the middleware chain step.
func (m *RateLimit) Middleware() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rc := schema.RequestContextFrom(r.Context())
			if rc == nil || rc.ParsedRequest == nil || !m.enabled {
				next.ServeHTTP(w, r)
				return
			}

			// a. RPM: one request unit, global then per-key.
			if !m.allowOr429(w, r, m.globalRPM, 1, "global") || !m.allowOr429(w, r, m.keyRPM(rc.VirtualKey), 1, rc.VirtualKey) {
				return
			}

			// b. TPM pre-check: reserve estimated prompt tokens.
			estimate := m.estimate.Estimate(rc.ParsedRequest)
			rc.TokensIn = estimate // the post-charge hook adjusts from here
			if !m.allowOr429(w, r, m.globalTPM, estimate, "global") || !m.allowOr429(w, r, m.keyTPM(rc.VirtualKey), estimate, rc.VirtualKey) {
				return
			}

			// c. TPM post-charge: after the response (including cache hits),
			// charge the actual tokens beyond the reserved estimate.
			defer func() {
				actual := rc.TokensIn + rc.TokensOut
				extra := actual - estimate
				if extra > 0 {
					_, _, _ = m.limiter.Allow(r.Context(), m.globalTPM, extra)
					_, _, _ = m.limiter.Allow(r.Context(), m.keyTPM(rc.VirtualKey), extra)
				}
			}()

			next.ServeHTTP(w, r)
		})
	}
}

// allowOr429 checks a dimension and writes a 429 rate_limit_error on denial.
// label names the subject in the message: a virtual key, or "global".
func (m *RateLimit) allowOr429(w http.ResponseWriter, r *http.Request, dimension string, cost int, label string) bool {
	allowed, _, err := m.limiter.Allow(r.Context(), dimension, cost)
	if err != nil {
		// A limiter failure must not block traffic; fail open.
		return true
	}
	if allowed {
		return true
	}
	schema.NewRateLimitError(m.message(dimension, label)).WriteJSON(w)
	return false
}

// message builds the error message in the §8.5 format:
// "Rate limit exceeded: 100 RPM for key gw-team-frontend".
func (m *RateLimit) message(dimension, label string) string {
	limit := 0
	if m.limitFor != nil {
		limit = m.limitFor(dimension)
	}
	unit := "RPM"
	if isTPMDimension(dimension) {
		unit = "TPM"
	}
	if label == "global" {
		return fmt.Sprintf("Rate limit exceeded: %d %s (global)", limit, unit)
	}
	return fmt.Sprintf("Rate limit exceeded: %d %s for key %s", limit, unit, label)
}

// isTPMDimension reports whether a dimension is a token (not request) limit.
func isTPMDimension(dimension string) bool {
	return len(dimension) >= 4 && dimension[len(dimension)-4:] == ":tpm"
}

// Estimator estimates prompt tokens before the request is sent (needed
// because rate limiting runs pre-request but token counts arrive
// post-response, §7 tpm_estimation).
type Estimator interface {
	Estimate(req *schema.UnifiedRequest) int
}

// CharEstimator implements "char_estimate": 1 token ≈ 4 characters.
// Fast and dependency-free; the "tokenizer" strategy (tiktoken) is not yet
// implemented — the wiring layer refuses to start if it is selected.
type CharEstimator struct{}

// Estimate returns len(all message text) / 4, rounded up.
func (CharEstimator) Estimate(req *schema.UnifiedRequest) int {
	chars := 0
	for _, msg := range req.Messages {
		for _, p := range schema.ContentParts(msg.Content) {
			if p.Type == "text" {
				chars += len(p.Text)
			}
		}
	}
	return (chars + 3) / 4
}
