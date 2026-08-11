package middleware

import (
	"net/http"
	"strconv"
	"time"

	"github.com/Jai-Keshav-Sharma/gatewai/internal/metrics"
	"github.com/Jai-Keshav-Sharma/gatewai/internal/schema"
)

// Metrics is middleware steps 3 and 10 of the chain (§4.1): start the timer
// pre-request, record latency/status/tokens/cost post-response. Because it
// wraps the cache middleware, cache hits are recorded too (with
// cache_hit=true, per §4.1 "steps 10-12 ALWAYS run").
func Metrics(enabled bool, pricer metrics.Pricer) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rc := schema.RequestContextFrom(r.Context())
			if rc == nil || !enabled {
				next.ServeHTTP(w, r)
				return
			}

			sw := &statusWriter{ResponseWriter: w}
			start := time.Now()
			next.ServeHTTP(sw, r)
			duration := time.Since(start)

			provider := rc.Provider
			if provider == "" {
				provider = "none" // rejected before routing (auth/ratelimit/etc.)
			}
			model := rc.Model
			if model == "" {
				model = "unknown"
			}

			metrics.RecordRequest(provider, model, strconv.Itoa(sw.status), rc.CacheHit, duration)
			metrics.RecordTokens(provider, model, rc.TokensIn, rc.TokensOut)
			if in, out, ok := pricer(provider, model); ok {
				metrics.RecordCost(provider, model, rc.TokensIn, rc.TokensOut, in, out)
			}
			if rc.VirtualKey != "" {
				metrics.RecordKey(rc.VirtualKey)
			}
		})
	}
}
