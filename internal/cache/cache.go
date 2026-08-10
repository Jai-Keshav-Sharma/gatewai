// Package cache stores and retrieves LLM responses (§4.1 step 6/9).
// Implementations: MemoryCache (in-process LRU) and RedisCache (distributed).
// SemanticCache extends Cache with vector similarity lookups.
package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/Jai-Keshav-Sharma/gatewai/internal/schema"
)

// Cache stores and retrieves LLM responses (plan §5.3 — exact definition).
type Cache interface {
	// Get retrieves a cached response for the given key.
	// Returns nil, false if not found or expired.
	Get(ctx context.Context, key string) (*schema.UnifiedResponse, bool)

	// Set stores a response with the given key and TTL.
	Set(ctx context.Context, key string, resp *schema.UnifiedResponse, ttl time.Duration) error

	// Close releases any resources held by the cache.
	Close() error
}

// SemanticCache extends Cache with vector similarity lookups (plan §5.3).
type SemanticCache interface {
	Cache

	// SearchSimilar finds cached responses whose prompt embedding is within
	// the similarity threshold. Returns the best match or nil.
	// threshold is a cosine similarity value between 0.0 and 1.0.
	SearchSimilar(ctx context.Context, embedding []float32, threshold float64) (*schema.UnifiedResponse, float64, bool)

	// SetWithEmbedding stores a response along with its prompt embedding vector.
	SetWithEmbedding(ctx context.Context, key string, embedding []float32, resp *schema.UnifiedResponse, ttl time.Duration) error
}

// Embedder generates vector embeddings from text (plan §5.3).
// Can be satisfied by the OpenAI adapter (calling /v1/embeddings) or any
// embedding service.
type Embedder interface {
	// Embed converts text into a vector embedding.
	Embed(ctx context.Context, text string) ([]float32, error)
}

// Key derives the cache key from a request: the SHA-256 of its canonical
// JSON. The request is stored as structs (no maps), so re-marshaling is
// deterministic — identical requests always produce identical keys.
func Key(req *schema.UnifiedRequest) string {
	data, err := json.Marshal(req)
	if err != nil {
		// Marshaling a parsed request cannot fail; fall back to the model name.
		data = []byte(req.Model)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// --- context plumbing between the cache middleware and the proxy ---
//
// The cache middleware wraps the proxy handler. On a MISS it must store the
// response AFTER the handler has produced it, and the proxy is the only
// place that sees the response (parsed JSON or translated SSE chunks). A
// context VALUE can't carry the response back out (the proxy's derived
// context is invisible to the middleware), so the middleware hands the proxy
// a shared SLOT — a pointer — that the proxy fills in and the middleware
// reads after the handler returns.

type responseSlot struct {
	mu sync.Mutex
	ur *schema.UnifiedResponse
}

// Set publishes the finished response.
func (s *responseSlot) Set(ur *schema.UnifiedResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ur = ur
}

// Get retrieves the published response (nil if none).
func (s *responseSlot) Get() *schema.UnifiedResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ur
}

type responseSlotKey struct{}

// WithResponseSlot attaches a shared response slot to the context.
func WithResponseSlot(ctx context.Context, s *responseSlot) context.Context {
	return context.WithValue(ctx, responseSlotKey{}, s)
}

// ResponseSlotFrom retrieves the slot attached by WithResponseSlot.
func ResponseSlotFrom(ctx context.Context) *responseSlot {
	s, _ := ctx.Value(responseSlotKey{}).(*responseSlot)
	return s
}

// NewResponseSlot creates an empty slot (for the middleware).
func NewResponseSlot() *responseSlot { return &responseSlot{} }

type accumulatorKey struct{}

// WithAccumulator attaches a stream accumulator to the context. The cache
// middleware creates it on a streaming MISS; the proxy tees every translated
// chunk into it; when the stream completes, the proxy fills the response
// slot with the reconstructed response for the middleware to store.
func WithAccumulator(ctx context.Context, a *Accumulator) context.Context {
	return context.WithValue(ctx, accumulatorKey{}, a)
}

// AccumulatorFrom retrieves the accumulator attached by WithAccumulator.
func AccumulatorFrom(ctx context.Context) *Accumulator {
	a, _ := ctx.Value(accumulatorKey{}).(*Accumulator)
	return a
}

// Accumulator reconstructs a UnifiedResponse from the translated OpenAI-format
// SSE chunks as they are streamed to the client (§4.3: "tee chunks to client
// AND accumulate in buffer simultaneously. After stream completes, store
// accumulated response in cache"). Content deltas are concatenated, usage is
// captured when a chunk carries it (OpenAI include_usage), and the last
// finish_reason wins.
type Accumulator struct {
	id           string
	model        string
	created      int64
	content      strings.Builder
	usage        *schema.Usage
	finishReason *string
}

// NewAccumulator creates an empty accumulator.
func NewAccumulator() *Accumulator { return &Accumulator{} }

// Add feeds one translated SSE data line ("data: {chunk-json}").
func (a *Accumulator) Add(data []byte) {
	line := data
	if len(line) > 5 && string(line[:5]) == "data:" {
		line = line[5:]
	}
	line = trimSpace(line)
	var chunk schema.StreamChunk
	if err := json.Unmarshal(line, &chunk); err != nil {
		return
	}
	if a.id == "" {
		a.id = chunk.ID
		a.model = chunk.Model
		a.created = chunk.Created
	}
	for _, c := range chunk.Choices {
		if delta, ok := c.Delta.(map[string]any); ok {
			if content, ok := delta["content"].(string); ok {
				a.content.WriteString(content)
			}
		}
		if c.FinishReason != nil {
			a.finishReason = c.FinishReason
		}
	}
	if chunk.Usage != nil {
		a.usage = chunk.Usage
	}
}

// Response returns the reconstructed response (id/model/finish_reason are
// taken from the chunks; the content is the concatenation of all deltas).
func (a *Accumulator) Response() *schema.UnifiedResponse {
	return &schema.UnifiedResponse{
		ID:      a.id,
		Object:  "chat.completion",
		Created: a.created,
		Model:   a.model,
		Choices: []schema.Choice{{
			Index:        0,
			Message:      &schema.Message{Role: "assistant", Content: a.content.String()},
			FinishReason: a.finishReason,
		}},
		Usage: a.usage,
	}
}

func trimSpace(b []byte) []byte {
	start := 0
	for start < len(b) && (b[start] == ' ' || b[start] == '\t') {
		start++
	}
	return b[start:]
}
