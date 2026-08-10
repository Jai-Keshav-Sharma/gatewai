package cache

import (
	"context"
	"math"
	"sync"
	"time"

	"github.com/Jai-Keshav-Sharma/gatewai/internal/schema"
)

// semanticEntry pairs a stored response with its prompt embedding.
type semanticEntry struct {
	embedding []float32
	resp      *schema.UnifiedResponse
	expiresAt time.Time
}

// MemorySemanticCache wraps any Cache with brute-force cosine similarity
// search over an in-memory vector store (§7 cache.semantic: "memory:
// brute-force cosine similarity ... fine for <10k cached entries").
// O(n) per search — acceptable for dev and single-instance use; Redis VSS is
// the O(log n) production alternative.
type MemorySemanticCache struct {
	inner Cache

	mu      sync.RWMutex
	vectors map[string]*semanticEntry
}

// NewMemorySemantic wraps a Cache with semantic lookups.
func NewMemorySemantic(inner Cache) *MemorySemanticCache {
	return &MemorySemanticCache{
		inner:   inner,
		vectors: make(map[string]*semanticEntry),
	}
}

// SearchSimilar returns the best stored response whose embedding is within
// the cosine-similarity threshold of the query embedding.
func (s *MemorySemanticCache) SearchSimilar(ctx context.Context, embedding []float32, threshold float64) (*schema.UnifiedResponse, float64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := time.Now()
	var best *schema.UnifiedResponse
	var bestSim float64
	for _, e := range s.vectors {
		if now.After(e.expiresAt) {
			continue
		}
		sim := cosine(e.embedding, embedding)
		if sim >= threshold && sim > bestSim {
			bestSim = sim
			best = e.resp
		}
	}
	if best == nil {
		return nil, 0, false
	}
	return best, bestSim, true
}

// SetWithEmbedding stores the response in the inner cache AND its embedding.
func (s *MemorySemanticCache) SetWithEmbedding(ctx context.Context, key string, embedding []float32, resp *schema.UnifiedResponse, ttl time.Duration) error {
	s.mu.Lock()
	s.vectors[key] = &semanticEntry{embedding: embedding, resp: resp, expiresAt: time.Now().Add(ttl)}
	s.mu.Unlock()
	return s.inner.Set(ctx, key, resp, ttl)
}

// Get delegates to the inner cache.
func (s *MemorySemanticCache) Get(ctx context.Context, key string) (*schema.UnifiedResponse, bool) {
	return s.inner.Get(ctx, key)
}

// Set delegates to the inner cache.
func (s *MemorySemanticCache) Set(ctx context.Context, key string, resp *schema.UnifiedResponse, ttl time.Duration) error {
	return s.inner.Set(ctx, key, resp, ttl)
}

// Close delegates to the inner cache.
func (s *MemorySemanticCache) Close() error { return s.inner.Close() }

// cosine computes the cosine similarity of two vectors (dot product over
// magnitudes). 1.0 = identical direction, 0.0 = orthogonal.
func cosine(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, magA, magB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		magA += float64(a[i]) * float64(a[i])
		magB += float64(b[i]) * float64(b[i])
	}
	if magA == 0 || magB == 0 {
		return 0
	}
	return dot / (math.Sqrt(magA) * math.Sqrt(magB))
}
