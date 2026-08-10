package ratelimit

import (
	"context"
	"strings"
	"sync"
	"time"
)

// MemoryLimiter is the in-process implementation: one token bucket per
// dimension (sync.Mutex guarded, §9). A token bucket refills at a steady
// rate up to a capacity: bursts up to the capacity are allowed, but the
// long-term average is capped at the refill rate. For an RPM/TPM-per-minute
// limit, capacity = limit and refill = limit per 60s.
//
// This is correct for a single gateway instance. Multi-instance deployments
// must use the RedisLimiter — otherwise each instance would have its own
// bucket and the combined throughput would exceed the configured limit
// (scalability principle: shared state across instances).
type MemoryLimiter struct {
	mu         sync.Mutex
	buckets    map[string]*bucket
	limits     map[string]int
	defaultRPM int // applied to unknown per-key dimensions (virtual keys disabled)
	defaultTPM int
}

// NewMemory builds a memory limiter with the given per-dimension limits
// (tokens per minute). Unknown RPM dimensions fall back to defaultRPM and
// unknown ":tpm" dimensions to defaultTPM — this is how per-key limits
// apply to arbitrary bearer keys when virtual keys are disabled.
func NewMemory(limits map[string]int, defaultRPM, defaultTPM int) *MemoryLimiter {
	buckets := make(map[string]*bucket, len(limits))
	for dim, limit := range limits {
		buckets[dim] = newBucket(limit)
	}
	return &MemoryLimiter{buckets: buckets, limits: limits, defaultRPM: defaultRPM, defaultTPM: defaultTPM}
}

// Allow checks and consumes cost units from the dimension's bucket.
func (l *MemoryLimiter) Allow(ctx context.Context, dimension string, cost int) (bool, int, error) {
	limit := l.defaultLimit(dimension)
	if limit <= 0 {
		return true, 0, nil // no limit configured for this dimension
	}
	l.mu.Lock()
	b, ok := l.buckets[dimension]
	if !ok {
		b = newBucket(limit)
		l.buckets[dimension] = b
	}
	l.mu.Unlock()
	allowed, remaining := b.consume(cost)
	return allowed, remaining, nil
}

// defaultLimit resolves the limit for a dimension: the configured limit, or
// the class default (":tpm" suffix selects the TPM default).
func (l *MemoryLimiter) defaultLimit(dimension string) int {
	if limit, ok := l.limits[dimension]; ok {
		return limit
	}
	if strings.HasSuffix(dimension, ":tpm") {
		return l.defaultTPM
	}
	return l.defaultRPM
}

// bucket is one token bucket. Not safe for concurrent use itself; the
// MemoryLimiter serializes access with its mutex.
type bucket struct {
	mu     sync.Mutex
	limit  int
	tokens float64
	last   time.Time
}

func newBucket(limit int) *bucket {
	return &bucket{limit: limit, tokens: float64(limit), last: time.Now()}
}

// consume refills from elapsed time, then spends cost tokens.
func (b *bucket) consume(cost int) (bool, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	elapsed := now.Sub(b.last).Seconds()
	b.last = now
	b.tokens += elapsed * float64(b.limit) / 60.0 // refill: limit per 60s
	if b.tokens > float64(b.limit) {
		b.tokens = float64(b.limit)
	}
	if b.tokens < float64(cost) {
		return false, int(b.tokens)
	}
	b.tokens -= float64(cost)
	return true, int(b.tokens)
}
