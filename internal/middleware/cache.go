package middleware

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/Jai-Keshav-Sharma/gatewai/internal/cache"
	"github.com/Jai-Keshav-Sharma/gatewai/internal/schema"
)

// Cache is middleware step 6 of the chain (§4.1): exact-match lookup
// pre-request; on a HIT, serve the cached response directly and skip the
// router entirely (steps 7-9) — but steps 10-12 (metrics, TPM post-charge,
// logging) still run because those middlewares are OUTSIDE this one. On a
// MISS, proceed to the provider; when the handler returns, store the
// response published by the proxy (parsed, or reconstructed from the
// streamed chunks via the accumulator).
type Cache struct {
	c           cache.Cache
	enabled     bool
	ttl         time.Duration
	semantic    bool
	threshold   float64
	embedder    cache.Embedder
	semanticTTL time.Duration
}

// NewCache builds the cache middleware.
func NewCache(c cache.Cache, enabled bool, ttl time.Duration, semanticEnabled bool, threshold float64, embedder cache.Embedder, semanticTTL time.Duration) *Cache {
	return &Cache{
		c:           c,
		enabled:     enabled,
		ttl:         ttl,
		semantic:    semanticEnabled,
		threshold:   threshold,
		embedder:    embedder,
		semanticTTL: semanticTTL,
	}
}

// Middleware implements the middleware chain step.
func (m *Cache) Middleware() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rc := schema.RequestContextFrom(r.Context())
			if rc == nil || rc.ParsedRequest == nil || !m.enabled {
				next.ServeHTTP(w, r)
				return
			}
			req := rc.ParsedRequest

			// Exact-match lookup first.
			key := cache.Key(req)
			if ur, hit := m.c.Get(r.Context(), key); hit {
				m.serveHit(w, rc, ur)
				return
			}

			// Semantic lookup (enabled explicitly): embed the prompt and
			// search for similar cached responses.
			if m.semantic && m.embedder != nil {
				sc, ok := m.c.(cache.SemanticCache)
				if ok {
					embedding, err := m.embedder.Embed(r.Context(), promptText(req))
					if err == nil {
						if ur, _, hit := sc.SearchSimilar(r.Context(), embedding, m.threshold); hit {
							m.serveHit(w, rc, ur)
							return
						}
					}
				}
			}

			// Streaming misses get an accumulator: the proxy tees every
			// translated chunk into it, then fills the response slot with the
			// reconstructed response for us to store.
			var acc *cache.Accumulator
			slot := cache.NewResponseSlot()
			ctx := r.Context()
			if req.Stream {
				acc = cache.NewAccumulator()
				ctx = cache.WithAccumulator(ctx, acc)
			}
			ctx = cache.WithResponseSlot(ctx, slot)

			next.ServeHTTP(w, r.WithContext(ctx))

			// Post-response store (only for successful 2xx responses — the
			// proxy only fills the slot for those).
			ur := slot.Get()
			if ur == nil {
				return
			}
			_ = m.c.Set(r.Context(), key, ur, m.ttl)
			if m.semantic && m.embedder != nil {
				if sc, ok := m.c.(cache.SemanticCache); ok {
					if embedding, err := m.embedder.Embed(r.Context(), promptText(req)); err == nil {
						_ = sc.SetWithEmbedding(r.Context(), key, embedding, ur, m.semanticTTL)
					}
				}
			}
		})
	}
}

// serveHit writes a cached response in the requested format (JSON or SSE)
// and marks the request as a cache hit. Token counts are recorded in the
// RequestContext so the TPM post-charge charges cache hits too.
func (m *Cache) serveHit(w http.ResponseWriter, rc *schema.RequestContext, ur *schema.UnifiedResponse) {
	rc.CacheHit = true
	if ur.Usage != nil {
		rc.TokensIn = ur.Usage.PromptTokens
		rc.TokensOut = ur.Usage.CompletionTokens
	}
	if rc.IsStreaming {
		writeSSE(w, ur)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(ur)
}

// writeSSE renders a cached response as an SSE stream (single content chunk
// + finish_reason, then the terminal marker).
func writeSSE(w http.ResponseWriter, ur *schema.UnifiedResponse) {
	fl, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	content := ""
	if len(ur.Choices) > 0 && ur.Choices[0].Message != nil {
		content, _ = ur.Choices[0].Message.Content.(string)
	}
	finish := "stop"
	if len(ur.Choices) > 0 && ur.Choices[0].FinishReason != nil {
		finish = *ur.Choices[0].FinishReason
	}
	chunk := schema.StreamChunk{
		ID:      ur.ID,
		Object:  "chat.completion.chunk",
		Created: ur.Created,
		Model:   ur.Model,
		Choices: []schema.StreamChoice{{
			Index:        0,
			Delta:        map[string]any{"content": content},
			FinishReason: &finish,
		}},
		Usage: ur.Usage,
	}
	line, _ := schema.MarshalSSE(chunk)
	_, _ = w.Write(line)
	_, _ = w.Write([]byte("\n"))
	_, _ = w.Write(schema.DoneSSE)
	if fl != nil {
		fl.Flush()
	}
}

// promptText flattens the conversation into text for embedding.
func promptText(req *schema.UnifiedRequest) string {
	var sb []byte
	for _, msg := range req.Messages {
		for _, p := range schema.ContentParts(msg.Content) {
			if p.Type == "text" {
				sb = append(sb, p.Text...)
			}
		}
	}
	return string(sb)
}
