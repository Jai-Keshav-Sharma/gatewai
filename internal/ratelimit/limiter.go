// Package ratelimit enforces request and token rate limits (§4.1 step 5).
// Two implementations of the Limiter interface: MemoryLimiter (in-process
// token buckets) and RedisLimiter (distributed sliding windows).
package ratelimit

import "context"

// Limiter checks and enforces rate limits (plan §5.4 — exact definition).
type Limiter interface {
	// Allow checks if the request is allowed under the rate limit.
	// dimension is the limit key (e.g., "global", "key:<hashed-key>").
	//   SECURITY: when using raw provider keys as dimensions, ALWAYS hash them
	//   first (SHA-256) to avoid leaking key material into Redis keys or map keys.
	// cost is the number of units consumed (1 for RPM, N for TPM where N = token count).
	// Returns (allowed bool, remaining int, err error).
	Allow(ctx context.Context, dimension string, cost int) (bool, int, error)
}
