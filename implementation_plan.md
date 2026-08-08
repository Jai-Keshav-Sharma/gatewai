# Gatewai — Final Implementation Plan

> **This document is the single source of truth.** Every decision is final. Every interface is exact. Every constraint is non-negotiable. Follow this document literally — do not deviate, do not improvise on architecture, do not add hardcoded logic.
>
> **Audience note:** this plan is written to be executed by an LLM coding agent. Every interface, file path, struct field, and rule below is a literal instruction. If a rule seems unusual, it is deliberate — implement it exactly as written and do not "simplify" it.

---

## 1. Project Identity

| Field | Value |
|:---|:---|
| **Name** | Gatewai |
| **Tagline** | High-performance AI Gateway |
| **What it is** | A reverse proxy that sits between applications and AI providers (OpenAI, Anthropic, Google Gemini), exposing a single unified OpenAI-compatible API |
| **Language** | Go 1.22+ |
| **License** | MIT |
| **Root directory** | `d:\gatewai` |
| **Go module path** | `github.com/gatewai/gatewai` (placeholder — update when repo is created) |

### What Problem It Solves

Applications talk to many AI providers, each with different APIs, auth mechanisms, and quirks. Gatewai gives them **one API** (OpenAI format) and handles routing, failover, caching, rate limiting, cost tracking, and content safety behind the scenes.

```
┌──────────┐       ┌───────────┐       ┌──────────┐
│ Your App │──────▶│  Gatewai  │──────▶│  OpenAI  │
│ (any     │  one  │  (proxy)  │       ├──────────┤
│ language)│  API  │           │──────▶│Anthropic │
└──────────┘       │           │       ├──────────┤
                   │           │──────▶│  Gemini  │
                   └───────────┘       └──────────┘
```

Users change ONE line in their existing code — the base URL — to point to Gatewai instead of the provider directly.

---

## 2. Performance Targets

These are hard targets. The architecture and code patterns in this document are specifically chosen to hit them.

| Metric | Target | How We Achieve It |
|:---|:---|:---|
| Concurrent requests | **10,000+** | Go goroutines (one per request, ~4KB stack each) |
| Added latency per request (P99) | **< 0.5ms** | sync.Pool buffer reuse, tuned connection pooling, zero unnecessary allocations on hot path |
| CPU overhead | **Negligible (I/O-bound)** | Gateway is I/O-bound (waiting on providers), not CPU-bound. CPU usage is dominated by upstream network I/O, not gateway processing. Measured as: gateway processing time per request < 0.5ms vs provider latency of 500ms–30s |
| Memory under 10k burst | **< 200MB** | Pooled 8KB buffers (10k × 8KB = 80MB), no request body copies unless caching is enabled |
| Codebase size | **< 8,000 lines** (excluding tests) | Clean interfaces, no code duplication, single responsibility per file |

---

## 3. Non-Negotiable Constraints

> [!CAUTION]
> These constraints apply to EVERY line of code in the project. Violating any of these is a bug.

### 3.1 Zero Hardcoding Policy
**Nothing is hardcoded.** Every behavior is configurable, pluggable, or driven by interfaces.

| What | How |
|:---|:---|
| Provider API URLs | Read from config YAML |
| Provider API keys | Read from environment variables (referenced in config via `${VAR_NAME}` syntax) |
| Rate limits (RPM, TPM) | Read from config YAML, per virtual key and global |
| Retry counts, timeouts | Read from config YAML |
| Cache TTLs, max sizes | Read from config YAML |
| Model names and pricing | Data structures in provider adapter files (not scattered in logic) |
| Guardrail rules | Pluggable classifier interface — NO regex, NO keyword lists, NO pattern matching |
| Load balancing strategy | Config YAML selects from registered strategies |
| Middleware ordering | Config YAML or explicit registration in main.go |

### 3.2 Design Principles (from *Designing Data-Intensive Applications* by Kleppmann)

Every architectural decision must serve at least one of these three:

1. **Reliability** — The system works correctly even when things go wrong.
   - Circuit breakers prevent cascading failures
   - Automatic failover between providers
   - Context cancellation prevents resource leaks when clients disconnect
   - Graceful shutdown completes in-flight requests

2. **Scalability** — The system handles growing load gracefully.
   - Stateless gateway → horizontal scaling by running more instances
   - Redis-backed state (cache, rate limits) shared across instances
   - sync.Pool prevents GC pressure under burst load
   - Connection pooling for upstream providers

3. **Maintainability** — Different people can work on the system productively.
   - Clean interface boundaries (adapter pattern for providers)
   - Middleware chain for composable features
   - Standard Go project layout
   - Every file has a single responsibility

### 3.3 Build & Learn Workflow
After completing each phase:
1. Walk the user through every module built in that phase
2. Explain what it does, how it works, and why it's designed that way
3. Give concrete examples (e.g., "when a request comes in, here's exactly what happens...")
4. Explain the system design principle at play

---

## 4. Architecture

### 4.1 Request Lifecycle (EXACT flow — do not deviate)

```
Client sends POST /v1/chat/completions
    │
    ▼
[HTTP Server] (net/http, tuned transport)
    │
    ▼
[Middleware Chain] — executed in this EXACT order:
    │
    ├── 0. Body parser (read body ONCE with pooled buffer, parse into UnifiedRequest,
    │       store in RequestContext — all downstream middleware consumes this,
    │       body is NOT re-read)
    ├── 1. Request ID injection (generate UUID, set X-Request-ID header)
    ├── 2. Structured logging (log request start)
    ├── 3. Metrics collection (start timer)
    ├── 4. Authentication (validate virtual key → resolve to provider key,
    │       check allowed_models against RequestContext.ParsedRequest.Model)
    ├── 5. Rate limiting:
    │       a. RPM check: 1 request unit (pre-request)
    │       b. TPM pre-check: estimate prompt tokens via tokenizer, reserve capacity
    │       c. TPM post-charge: after response, charge actual tokens consumed,
    │          adjust reservation (runs as deferred post-response hook)
    ├── 6. Cache lookup (exact match → semantic match)
    │       uses RequestContext.ParsedRequest to generate cache key
    │       ├── HIT → return cached response, skip steps 7-9 ONLY.
    │       │     Steps 10-12 (metrics, TPM post-charge, logging) ALWAYS run
    │       │     with cache_hit=true flag set in RequestContext.
    │       └── MISS → continue
    ├── 7. Pre-request guardrail (evaluate prompt via classifier,
    │       reads messages from RequestContext.ParsedRequest)
    │
    ▼
[Router] — select provider INSTANCE + API key
    │
    ├── Resolve requested model → candidate instances (instances whose `models` list contains it)
    ├── Filter OUT instances whose circuit breaker is OPEN
    ├── Apply load balancing strategy (round-robin / weighted / least-latency) to pick ONE instance
    ├── On failure, apply the RETRY POLICY and FAILOVER SEQUENCE below
    │       ├── RETRY POLICY (non-negotiable):
    │       │     • Retry ON: connection errors, timeouts, 5xx, 429
    │       │     • Retry ONLY IF: no response bytes have been sent to client yet
    │       │     • NEVER retry: 4xx (except 429), or mid-stream (chunks already sent)
    │       │     • Honor Retry-After header on 429 responses
    │       │     • Use exponential backoff with jitter between retries
    │       └── FAILOVER SEQUENCE (exact order — non-negotiable):
    │             1. SAME-TYPE RETRY: retry the SAME model on other healthy instances of the
    │                SAME provider type (e.g., openai-1 → openai-2). This handles per-key rate
    │                limits. Respect each instance's max_retries.
    │             2. CROSS-TYPE FAILOVER: if all instances of the current type are exhausted,
    │                move to the NEXT TYPE in routing.fallback_order. Translate the model via
    │                model_mapping (e.g., gpt-4o → claude-sonnet-4-20250514 for type "anthropic").
    │                Then repeat step 1 within the new type.
    │             3. If every type in fallback_order is exhausted → return 502 provider_error.
    │
    │       NOTE: routing.fallback_order lists provider TYPES, not instance names. All healthy
    │       instances of a type are candidates (selected via strategy) before moving to the
    │       next type. Adding a new instance to config automatically joins the failover pool.
    │
    ▼
[Provider Adapter] — translate request format
    │
    ├── Convert UnifiedRequest → provider-native HTTP request
    ├── Set provider-specific headers and auth
    │
    ▼
[Upstream Provider] (OpenAI / Anthropic / Gemini)
    │
    ▼
[Provider Adapter] — translate response format
    │
    ├── Convert provider-native response → UnifiedResponse
    ├── For streaming: translate provider SSE chunks → OpenAI SSE chunks
    │
    ▼
[Middleware Chain continues — post-processing]:
    │
    ├── 8. Post-response guardrail:
    │       • NON-STREAMING: evaluate full response text via classifier
    │       • STREAMING: SKIPPED by default (chunks already sent to client).
    │         Opt-in "buffer mode": accumulate all chunks, evaluate full text,
    │         then send to client (kills streaming latency — user must explicitly enable)
    ├── 9. Cache store:
    │       • NON-STREAMING: store full response
    │       • STREAMING: tee chunks to client AND accumulate in buffer simultaneously.
    │         After stream completes, store accumulated response in cache.
    ├── 10. Metrics collection (record latency, tokens, cost):
    │       • NON-STREAMING: read tokens from response Usage field
    │       • STREAMING (OpenAI): if the client did NOT set stream_options.include_usage,
    │         inject include_usage=true into the upstream request so usage arrives in the
    │         final chunk. STRIPPING RULE: an injected usage chunk MUST be consumed for
    │         metrics and MUST NOT be forwarded to the client. If the client originally
    │         requested include_usage, forward the usage chunk as-is.
    │       • STREAMING (Anthropic): parse usage from message_start (input tokens)
    │         and message_delta (output tokens) events.
    │       • STREAMING (Gemini): parse usageMetadata from the final chunk.
    ├── 11. TPM post-charge: charge actual tokens consumed, adjust reservation.
    │       • CACHE HITS ARE CHARGED TOO (non-negotiable): a cache hit still post-charges
    │         the cached response's token counts. This prevents the cache from becoming
    │         a rate-limit bypass.
    ├── 12. Structured logging (log request completion)
    │
    ▼
Client receives response
```

### 4.2 Streaming (SSE) Flow

When `"stream": true` is set in the request:

```
Client ←──SSE──── Gatewai ←──SSE──── Provider

1. Gatewai receives provider SSE chunks one at a time
2. Each chunk is translated from provider format → OpenAI format
3. Translated chunk is written to client response writer
4. http.Flusher.Flush() is called IMMEDIATELY after each chunk write
5. If caching is enabled: chunk is ALSO written to an accumulation buffer (tee)
6. If client disconnects (context cancelled), upstream request is cancelled immediately
7. The final chunk is "data: [DONE]\n\n"
8. After stream completes: accumulated buffer is stored in cache (if enabled)
```

**SSE Response Headers** (set these exactly):
```http
Content-Type: text/event-stream; charset=utf-8
Cache-Control: no-cache, no-transform
Connection: keep-alive
X-Accel-Buffering: no
```

**SSE Chunk Format** (each chunk is exactly this):
```
data: {"id":"chatcmpl-xxx","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}\n\n
```

**Terminal Chunk**:
```
data: [DONE]\n\n
```

### 4.3 Streaming-Aware Middleware Rules

> [!CAUTION]
> Streaming fundamentally changes how post-processing middleware works. These rules are non-negotiable.

| Middleware | Non-Streaming Behavior | Streaming Behavior |
|:---|:---|:---|
| **Body parser** | Parse body once, store in context | Same — parsing happens before streaming decision |
| **Auth** | Check virtual key + allowed_models | Same — runs pre-request |
| **Rate limit (RPM)** | Charge 1 request pre-request | Same |
| **Rate limit (TPM)** | Estimate prompt tokens pre-request, charge actual post-response | Estimate pre-request. Post-charge after stream completes using usage from final SSE chunk |
| **Cache lookup** | Check cache pre-request | Same |
| **Cache store** | Store full response post-response | Tee chunks to client + buffer simultaneously. Store accumulated response after `[DONE]` |
| **Pre-request guardrail** | Evaluate prompt before provider call | Same — runs pre-request |
| **Post-response guardrail** | Evaluate full response text | **SKIP by default** (chunks already sent). Opt-in `buffer_mode: true` accumulates entire response before sending |
| **Metrics** | Read Usage from response body | OpenAI: inject `include_usage: true` if the client didn't set it, and STRIP the injected usage chunk from the client stream. Anthropic: parse `message_start` + `message_delta` usage events. Gemini: parse `usageMetadata` from final chunk |
| **Logger** | Log after response is written | Log after stream completes |

---

## 5. Core Interfaces (EXACT definitions — implement these literally)

### 5.1 Provider Interface

```go
package provider

import (
    "context"
    "net/http"
    
    "github.com/gatewai/gatewai/internal/schema"
)

// Provider adapts a specific AI provider's API to Gatewai's unified format.
// Each provider (OpenAI, Anthropic, Gemini) implements this interface.
type Provider interface {
    // Name returns the provider identifier (e.g., "openai", "anthropic", "gemini").
    Name() string

    // BuildRequest converts a unified request into a provider-native HTTP request.
    // This includes setting the correct URL, headers, auth, and body format.
    BuildRequest(ctx context.Context, req *schema.UnifiedRequest, apiKey string) (*http.Request, error)

    // ParseResponse reads the provider's HTTP response and converts it to unified format.
    // Called only for non-streaming responses.
    ParseResponse(ctx context.Context, resp *http.Response) (*schema.UnifiedResponse, error)

    // TranslateStreamChunk converts a single provider-specific SSE data line
    // into an OpenAI-format SSE data line.
    // Returns nil if the chunk should be skipped (e.g., provider-specific metadata events).
    TranslateStreamChunk(ctx context.Context, chunk []byte) ([]byte, error)

    // Models returns the list of models this provider supports, with pricing info.
    Models() []schema.Model
    
    // SupportsStreaming returns true if this provider supports SSE streaming.
    SupportsStreaming() bool
}
```

### 5.2 Middleware Interface

```go
package middleware

import "net/http"

// Middleware wraps an http.Handler with additional behavior.
// It receives the next handler in the chain and returns a new handler.
type Middleware func(next http.Handler) http.Handler

// Chain composes middlewares around a base handler.
// The first middleware in the slice is the outermost (executes first on request,
// last on response). This ordering matters for correctness.
func Chain(base http.Handler, mws ...Middleware) http.Handler {
    // Apply in reverse so the first middleware in the slice wraps outermost
    for i := len(mws) - 1; i >= 0; i-- {
        base = mws[i](base)
    }
    return base
}
```

### 5.3 Cache Interface

```go
package cache

import (
    "context"
    "time"
    
    "github.com/gatewai/gatewai/internal/schema"
)

// Cache stores and retrieves LLM responses.
// Two implementations: MemoryCache (in-process LRU) and RedisCache (distributed).
type Cache interface {
    // Get retrieves a cached response for the given key.
    // Returns nil, false if not found or expired.
    Get(ctx context.Context, key string) (*schema.UnifiedResponse, bool)

    // Set stores a response with the given key and TTL.
    Set(ctx context.Context, key string, resp *schema.UnifiedResponse, ttl time.Duration) error

    // Close releases any resources held by the cache.
    Close() error
}

// SemanticCache extends Cache with vector similarity lookups.
// Used for finding cached responses to semantically similar (but not identical) prompts.
type SemanticCache interface {
    Cache

    // SearchSimilar finds cached responses whose prompt embedding is within
    // the similarity threshold. Returns the best match or nil.
    // threshold is a cosine similarity value between 0.0 and 1.0.
    SearchSimilar(ctx context.Context, embedding []float32, threshold float64) (*schema.UnifiedResponse, float64, bool)

    // SetWithEmbedding stores a response along with its prompt embedding vector.
    SetWithEmbedding(ctx context.Context, key string, embedding []float32, resp *schema.UnifiedResponse, ttl time.Duration) error
}

// Embedder generates vector embeddings from text.
// Used by SemanticCache to convert prompts into vectors for similarity search.
// Can be satisfied by the OpenAI adapter (calling /v1/embeddings) or any embedding service.
type Embedder interface {
    // Embed converts text into a vector embedding.
    Embed(ctx context.Context, text string) ([]float32, error)
}
```

### 5.4 Rate Limiter Interface

```go
package ratelimit

import "context"

// Limiter checks and enforces rate limits.
// Two implementations: MemoryLimiter (in-process) and RedisLimiter (distributed).
type Limiter interface {
    // Allow checks if the request is allowed under the rate limit.
    // dimension is the limit key (e.g., "global", "key:<hashed-key>").
    //   SECURITY: when using raw provider keys as dimensions, ALWAYS hash them
    //   first (SHA-256) to avoid leaking key material into Redis keys or map keys.
    // cost is the number of units consumed (1 for RPM, N for TPM where N = token count).
    // Returns (allowed bool, remaining int, err error).
    Allow(ctx context.Context, dimension string, cost int) (bool, int, error)
}
```

### 5.5 Guard Interface (Guardrails)

```go
package guardrail

import (
    "context"

    "github.com/gatewai/gatewai/internal/schema"
)

// Verdict represents the result of a guard evaluation.
type Verdict struct {
    Safe       bool    // true if content passed the check
    Reason     string  // human-readable explanation (empty if safe)
    Confidence float64 // 0.0 to 1.0, how confident the classifier is
    GuardName  string  // which guard produced this verdict
}

// Guard evaluates content for safety/compliance.
// Three implementations: LLMGuard, WebhookGuard, ProviderGuard.
// NO hardcoded regex, keyword lists, or pattern matching — ever.
type Guard interface {
    // Name returns the guard identifier (e.g., "openai-moderation", "custom-webhook").
    Name() string

    // EvaluateRequest checks the user's prompt before it's sent to a provider.
    // Return a Verdict. If Safe=false, the request is rejected with the Reason.
    EvaluateRequest(ctx context.Context, messages []schema.Message) (*Verdict, error)

    // EvaluateResponse checks the provider's response before it's returned to the client.
    // Return a Verdict. If Safe=false, the response is replaced with an error message.
    EvaluateResponse(ctx context.Context, content string) (*Verdict, error)
}
```

### 5.6 Router / Load Balancer Interface

```go
package router

import (
    "context"
    "time"
)

// Endpoint represents a single provider INSTANCE + model combination that can serve requests.
type Endpoint struct {
    ProviderName string // provider INSTANCE name from config (e.g., "openai-1"), NOT the type
    APIKey       string // the actual provider API key (resolved from virtual key or config)
    Model        string // model to request from this endpoint (after model_mapping translation)
}

// Strategy selects an endpoint from a list of candidates.
// Three implementations: RoundRobin, Weighted, LeastLatency.
type Strategy interface {
    // Name returns the strategy identifier (e.g., "round-robin", "weighted", "least-latency").
    Name() string

    // Select picks the best endpoint from the candidates.
    // Must be safe for concurrent use.
    Select(ctx context.Context, candidates []Endpoint) (*Endpoint, error)
}

// LatencyTracker records and reports per-endpoint latency using
// Exponentially Weighted Moving Average (EWMA).
// The proxy handler calls Record() after every request.
// The LeastLatency strategy calls Get() during selection.
type LatencyTracker interface {
    // Record stores a latency observation for the given endpoint.
    // endpointName is the provider INSTANCE name (e.g., "openai-1").
    Record(endpointName string, latency time.Duration)

    // Get returns the current EWMA latency for the given endpoint instance.
    // Returns 0 if no observations have been recorded.
    Get(endpointName string) time.Duration
}
```

---

## 6. Shared Schema Types (EXACT definitions)

```go
package schema

import (
    "encoding/json"
    "time"
)

// UnifiedRequest is the internal representation of a chat completion request.
// It mirrors the OpenAI format since that's our canonical API.
type UnifiedRequest struct {
    Model            string         `json:"model"`
    Messages         []Message      `json:"messages"`
    Temperature      *float64       `json:"temperature,omitempty"`
    TopP             *float64       `json:"top_p,omitempty"`
    N                *int           `json:"n,omitempty"`
    Stream           bool           `json:"stream"`
    StreamOptions    *StreamOptions `json:"stream_options,omitempty"`
    Stop             StringOrArray  `json:"stop,omitempty"`
    MaxTokens        *int           `json:"max_tokens,omitempty"`
    PresencePenalty  *float64       `json:"presence_penalty,omitempty"`
    FrequencyPenalty *float64       `json:"frequency_penalty,omitempty"`
    Tools            []Tool         `json:"tools,omitempty"`
    ToolChoice       any            `json:"tool_choice,omitempty"`
    ResponseFormat   *ResponseFormat `json:"response_format,omitempty"`
}

type Message struct {
    Role       string     `json:"role"`                  // "system", "user", "assistant", "tool"
    Content    any        `json:"content"`               // string or []ContentPart
    Name       string     `json:"name,omitempty"`
    ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
    ToolCallID string     `json:"tool_call_id,omitempty"`
}

type StreamOptions struct {
    IncludeUsage bool `json:"include_usage,omitempty"`
}

// StringOrArray accepts either a bare JSON string or an array of strings.
// OpenAI allows BOTH forms for the "stop" field ("stop": "END" or "stop": ["A","B"]).
// A plain []string would reject the bare-string form with a 400 — breaking
// OpenAI compatibility. Adapters treat StringOrArray as a plain []string.
type StringOrArray []string

// UnmarshalJSON implements the exact normalization rule: an array stays an array,
// a bare string becomes a single-element array.
func (s *StringOrArray) UnmarshalJSON(data []byte) error {
    var arr []string
    if err := json.Unmarshal(data, &arr); err == nil {
        *s = arr
        return nil
    }
    var single string
    if err := json.Unmarshal(data, &single); err != nil {
        return err
    }
    *s = []string{single}
    return nil
}

type Tool struct {
    Type     string   `json:"type"` // always "function"
    Function Function `json:"function"`
}

type Function struct {
    Name        string `json:"name"`
    Description string `json:"description,omitempty"`
    Parameters  any    `json:"parameters,omitempty"` // JSON Schema object
}

type ToolCall struct {
    ID       string       `json:"id"`
    Type     string       `json:"type"` // "function"
    Function FunctionCall `json:"function"`
}

type FunctionCall struct {
    Name      string `json:"name"`
    Arguments string `json:"arguments"` // JSON string
}

type ResponseFormat struct {
    Type       string `json:"type"` // "text", "json_object", "json_schema"
    JSONSchema any    `json:"json_schema,omitempty"`
}

// UnifiedResponse is the internal representation of a chat completion response.
type UnifiedResponse struct {
    ID      string   `json:"id"`
    Object  string   `json:"object"` // "chat.completion"
    Created int64    `json:"created"`
    Model   string   `json:"model"`
    Choices []Choice `json:"choices"`
    Usage   *Usage   `json:"usage,omitempty"`
}

type Choice struct {
    Index        int      `json:"index"`
    Message      *Message `json:"message,omitempty"`      // non-streaming
    Delta        *Message `json:"delta,omitempty"`         // streaming
    FinishReason *string  `json:"finish_reason,omitempty"` // "stop", "length", "tool_calls", null
}

type Usage struct {
    PromptTokens     int `json:"prompt_tokens"`
    CompletionTokens int `json:"completion_tokens"`
    TotalTokens      int `json:"total_tokens"`
}

// Model describes a model supported by a provider.
type Model struct {
    ID              string  `json:"id"`       // e.g., "gpt-4o"
    Provider        string  `json:"provider"` // e.g., "openai"
    InputPricePer1M  float64 // USD per 1M input tokens
    OutputPricePer1M float64 // USD per 1M output tokens
    ContextWindow   int     // max tokens (input + output)
}

// RequestContext carries metadata through the middleware chain.
// Lives in internal/schema/context.go — NOT in internal/proxy. The schema package
// imports nothing, so every package (middleware, proxy, router) can depend on it
// without circular imports. Stored in context.Context via context.WithValue using
// the helpers WithRequestContext / RequestContextFrom defined in the same file.
type RequestContext struct {
    RequestID     string
    VirtualKey    string           // the virtual key used (e.g., "gw-abc123")
    ResolvedKey   string           // the actual provider API key
    Provider      string           // selected provider name
    Model         string           // requested model
    StartTime     time.Time        // when the request was received
    CacheHit      bool             // whether the response came from cache
    TokensIn      int              // prompt tokens (from response)
    TokensOut     int              // completion tokens (from response)
    IsStreaming   bool             // whether this is a streaming request
    ParsedRequest *UnifiedRequest  // parsed request body (set by body parser middleware)
    RawBody       []byte           // raw request body bytes (for forwarding to provider)
}
```

---

## 7. Configuration Schema (EXACT YAML structure)

This is the config file users will write. Every field listed here must be supported.

```yaml
# gatewai.yaml — Gatewai configuration file

server:
  host: "0.0.0.0"           # bind address
  port: 8080                 # listen port
  read_timeout: "30s"        # max time to read request (Go duration string)
  write_timeout: "0s"        # MUST be 0 (disabled) — Go's WriteTimeout covers the ENTIRE
                              # response lifetime. LLM streams can last minutes (long reasoning).
                              # A non-zero value will sever active streams.
                              # We rely on per-provider upstream timeouts + context cancellation instead.
  graceful_shutdown: "30s"   # time to wait for in-flight requests on shutdown

providers:
  - name: "openai-1"               # UNIQUE instance name (used in routing, logs, metrics)
    type: "openai"                  # provider type (selects which adapter to use)
    api_key: "${OPENAI_API_KEY_1}"  # environment variable interpolation
    base_url: "https://api.openai.com/v1"  # overridable for Azure, proxies, etc.
    models:
      - "gpt-4o"
      - "gpt-4o-mini"
    weight: 1                # for weighted load balancing (higher = more traffic)
    max_retries: 2           # per-provider retry count
    timeout: "60s"           # per-request timeout to this provider

  - name: "openai-2"               # second OpenAI instance with different key
    type: "openai"                  # same adapter, different credentials
    api_key: "${OPENAI_API_KEY_2}"
    base_url: "https://api.openai.com/v1"
    models:
      - "gpt-4o"
    weight: 2                # gets 2x traffic vs openai-1
    max_retries: 2
    timeout: "60s"

  - name: "anthropic-1"
    type: "anthropic"
    api_key: "${ANTHROPIC_API_KEY}"
    base_url: "https://api.anthropic.com"
    models:
      - "claude-sonnet-4-20250514"
      - "claude-haiku-4-20250414"
    weight: 1
    max_retries: 2
    timeout: "60s"
    default_max_tokens: 8192  # Anthropic's API REQUIRES max_tokens in every request.
                              # RULE: only adapters whose provider API mandates max_tokens
                              # (currently: Anthropic ONLY) inject this value when the client
                              # omits max_tokens. OpenAI/Gemini adapters MUST pass max_tokens
                              # through exactly as received (including omitted) — injecting a
                              # cap would silently truncate responses.
                              # VALIDATION: default_max_tokens is REQUIRED when type="anthropic"
                              # and MUST NOT be set for any other provider type.

  - name: "gemini-1"
    type: "gemini"
    api_key: "${GEMINI_API_KEY}"
    base_url: "https://generativelanguage.googleapis.com"
    models:
      - "gemini-2.5-pro"
      - "gemini-2.5-flash"
    weight: 1
    max_retries: 2
    timeout: "60s"

# Model mapping for cross-provider failover.
# When a provider is down and we fail over, this maps the requested model
# to an equivalent model on the fallback provider.
# Without this, failover is broken for every model-specific request.
# Keys under each model are provider TYPES (not instance names) — any healthy
# instance of that type can serve the mapped model.
model_mapping:
  gpt-4o:
    anthropic: "claude-sonnet-4-20250514"
    gemini: "gemini-2.5-pro"
  gpt-4o-mini:
    anthropic: "claude-haiku-4-20250414"
    gemini: "gemini-2.5-flash"
  claude-sonnet-4-20250514:
    openai: "gpt-4o"
    gemini: "gemini-2.5-pro"
  claude-haiku-4-20250414:
    openai: "gpt-4o-mini"
    gemini: "gemini-2.5-flash"
  gemini-2.5-pro:
    openai: "gpt-4o"
    anthropic: "claude-sonnet-4-20250514"
  gemini-2.5-flash:
    openai: "gpt-4o-mini"
    anthropic: "claude-haiku-4-20250414"

routing:
  strategy: "round-robin"       # "round-robin" | "weighted" | "least-latency"
  fallback_order:               # provider TYPES (NOT instance names). On failure, all healthy
    - "openai"                  # instances of the current type are retried FIRST (same-type retry),
    - "anthropic"               # then the next TYPE in this list, translating the model via
    - "gemini"                  # model_mapping. See FAILOVER SEQUENCE in Section 4.1.
  circuit_breaker:
    failure_threshold: 5        # consecutive failures before opening circuit
    recovery_timeout: "30s"     # how long to wait before trying half-open
    half_open_max_requests: 3   # requests to allow in half-open state

rate_limiting:
  enabled: true
  backend: "memory"             # "memory" | "redis"
  # TPM estimation strategy: how to estimate prompt tokens BEFORE the request is sent.
  # This is needed because rate limiting runs pre-request but token counts are post-response.
  # Options:
  #   "char_estimate" — approximate: 1 token ≈ 4 characters (fast, no dependencies)
  #   "tokenizer"     — accurate: uses tiktoken-go for OpenAI, approximations for others
  tpm_estimation: "char_estimate"
  global:
    rpm: 1000                   # requests per minute (all keys combined)
    tpm: 500000                 # tokens per minute (all keys combined)
  per_key:
    rpm: 100                    # default per virtual key
    tpm: 50000                  # default per virtual key

cache:
  enabled: true
  backend: "memory"             # "memory" | "redis"
  exact_match:
    ttl: "1h"                   # how long to cache exact matches
    max_entries: 10000          # max items in cache (LRU eviction)
  semantic:
    enabled: false              # works with BOTH memory and redis backends
    similarity_threshold: 0.92  # cosine similarity threshold (0.0 - 1.0)
    embedding_provider: "openai-1" # provider INSTANCE name (NOT type) — API keys live on
                                   # instances. Calls /v1/embeddings on that instance's
                                   # base_url with that instance's key.
    embedding_model: "text-embedding-3-small"
    ttl: "24h"
    # Backend behavior:
    # - memory: brute-force cosine similarity over in-memory vector store
    #           (fine for <10k cached entries, good for dev/single-instance)
    # - redis:  Redis VSS (Vector Similarity Search) for O(log n) lookups
    #           (required for production/multi-instance at scale)

redis:                           # only needed if any backend is set to "redis"
  address: "localhost:6379"
  password: "${REDIS_PASSWORD}"
  db: 0
  pool_size: 100

virtual_keys:
  enabled: true
  keys:
    - key: "gw-team-frontend"
      description: "Frontend team"
      allowed_models: ["gpt-4o-mini", "claude-haiku-4-20250414"]
      rate_limit:
        rpm: 50
        tpm: 25000
    - key: "gw-team-backend"
      description: "Backend team"
      allowed_models: ["*"]     # all models
      rate_limit:
        rpm: 200
        tpm: 100000

guardrails:
  pre_request: []               # guards to run before sending to provider
  post_response: []             # guards to run before returning to client
  buffer_mode: false            # if true, streaming responses are fully buffered
                                 # before post-response guards run.
                                 # if false (default), post-response guards are SKIPPED
                                 # for streaming requests (chunks already sent to client).
  # Example guard configurations:
  # - type: "llm"              # use an LLM as a classifier
  #   provider: "openai-1"     # provider INSTANCE name (NOT type) — keys live on instances
  #   model: "gpt-4o-mini"
  #   prompt: "Evaluate if the following content is safe..."
  #   threshold: 0.8
  # - type: "webhook"          # call an external classifier service
  #   url: "http://safety-service:8000/classify"
  #   timeout: "2s"
  # - type: "provider"         # use provider's built-in moderation
  #   provider: "openai-1"     # INSTANCE name — calls that instance's /v1/moderations endpoint

logging:
  level: "info"                 # "debug" | "info" | "warn" | "error"
  format: "json"                # "json" | "text"

metrics:
  enabled: true
  path: "/metrics"              # Prometheus scrape endpoint
```

### Environment Variable Interpolation

Config values containing `${VAR_NAME}` MUST be resolved from environment variables at config load time. Rules:
- If a referenced variable is not set AND the feature using it is **enabled**, Gatewai MUST fail to start with a clear error message.
- If a referenced variable is not set BUT the feature using it is **disabled** (e.g., `redis.password` when `cache.backend: "memory"`), the variable is **not resolved** and no error is raised.
- Never silently use an empty string for a missing required variable.

---

## 8. API Contract (EXACT endpoints)

### 8.1 Endpoints

| Method | Path | Purpose |
|:---|:---|:---|
| `POST` | `/v1/chat/completions` | Chat completion (streaming and non-streaming) |
| `GET` | `/v1/models` | List all available models across all providers |
| `GET` | `/health` | Health check (returns `200 OK` with `{"status": "ok"}`) |
| `GET` | `/metrics` | Prometheus metrics scrape endpoint |

### 8.2 Authentication

All `/v1/*` endpoints require an `Authorization: Bearer <key>` header. The key is either:
- A virtual key (e.g., `gw-team-frontend`) — resolved to a provider key by the auth middleware
- A provider key directly (if virtual keys are disabled)

### 8.3 Request Format

Identical to the OpenAI Chat Completions API. See `UnifiedRequest` struct in Section 6.

### 8.4 Response Format

Identical to the OpenAI Chat Completions API. See `UnifiedResponse` struct in Section 6.

### 8.5 Error Response Format

```json
{
    "error": {
        "message": "Rate limit exceeded: 100 RPM for key gw-team-frontend",
        "type": "rate_limit_error",
        "code": "rate_limit_exceeded"
    }
}
```

Error types and HTTP status codes:
| Error Type | HTTP Status | When |
|:---|:---|:---|
| `invalid_request_error` | 400 | Malformed request body |
| `authentication_error` | 401 | Missing or invalid API key |
| `permission_error` | 403 | Key not allowed to use requested model |
| `rate_limit_error` | 429 | RPM or TPM limit exceeded |
| `provider_error` | 502 | All providers failed (after retries and fallback) |
| `guardrail_blocked` | 400 | Content blocked by guardrail classifier |
| `internal_error` | 500 | Unexpected gateway error |

---

## 9. Code Organization (EXACT file tree)

Every file listed below must be created. No additional files should be created in the core application unless they serve a purpose listed here.

```
d:\gatewai\
│
├── cmd/
│   └── gatewai/
│       └── main.go                          # Entry point: load config, wire dependencies, start server
│
├── internal/
│   ├── config/
│   │   ├── config.go                        # Config struct, YAML loading, env var interpolation
│   │   └── validate.go                      # Config validation: required fields, valid values, reference
│   │   │                                    # integrity (fallback_order entries must be valid provider TYPES;
│   │   │                                    # embedding_provider and guardrail providers must be valid INSTANCE
│   │   │                                    # names; default_max_tokens REQUIRED for anthropic-type instances
│   │   │                                    # and MUST NOT be set for other types)
│   │
│   ├── server/
│   │   ├── server.go                        # HTTP server: net/http.Server with tuned timeouts, graceful shutdown
│   │   └── routes.go                        # Route registration: mux setup, middleware chain composition
│   │
│   ├── proxy/
│   │   ├── handler.go                       # Core proxy handler (the hot path): dispatch to router, write response
│   │   └── stream.go                        # SSE streaming: line buffering, chunk flushing, context cancellation
│   │
│   ├── schema/
│   │   ├── request.go                       # UnifiedRequest, Message, Tool, ToolCall, StringOrArray, etc.
│   │   ├── response.go                      # UnifiedResponse, Choice, Usage, etc.
│   │   ├── model.go                         # Model struct (name, pricing, context window)
│   │   ├── context.go                       # RequestContext struct + WithRequestContext / RequestContextFrom helpers
│   │   └── errors.go                        # Error types: GatewaiError struct, error constructors
│   │
│   ├── provider/
│   │   ├── provider.go                      # Provider interface definition
│   │   ├── registry.go                      # Provider registry: map[string]Provider keyed by INSTANCE name,
│   │   │                                    # with type→adapter mapping for shared adapter reuse
│   │   ├── openai/
│   │   │   ├── adapter.go                   # OpenAI adapter (mostly passthrough since our format IS OpenAI)
│   │   │   ├── models.go                    # OpenAI model catalog with pricing
│   │   │   └── stream.go                    # OpenAI SSE handling (passthrough — format matches)
│   │   ├── anthropic/
│   │   │   ├── adapter.go                   # Anthropic adapter: translate messages, system prompt, tools
│   │   │   ├── models.go                    # Anthropic model catalog with pricing
│   │   │   └── stream.go                    # Anthropic SSE → OpenAI SSE translation
│   │   └── gemini/
│   │       ├── adapter.go                   # Gemini adapter: translate messages, tools, response format
│   │       ├── models.go                    # Gemini model catalog with pricing
│   │       └── stream.go                    # Gemini SSE → OpenAI SSE translation
│   │
│   ├── router/
│   │   ├── router.go                        # Router: resolves model → candidate INSTANCES, applies strategy, handles failover
│   │   ├── strategy.go                      # Load balancing strategies: RoundRobin, Weighted, LeastLatency
│   │   ├── failover.go                      # Retry with exponential backoff + jitter. FAILOVER SEQUENCE:
│   │   │                                    # same-type instances first, then next type in fallback_order
│   │   │                                    # with model_mapping. Retry policy: only on conn
│   │   │                                    # errors/timeouts/5xx/429, NEVER mid-stream
│   │   ├── circuit.go                       # Circuit breaker: Closed/Open/HalfOpen states, atomic counters
│   │   ├── modelmapping.go                  # Cross-provider model equivalence resolution
│   │   └── latencytracker.go                # EWMA latency tracker for least-latency strategy
│   │
│   ├── middleware/
│   │   ├── chain.go                         # Middleware chain composer (the Chain function)
│   │   ├── bodyparser.go                    # Step 0: read body ONCE, parse into UnifiedRequest, store in context
│   │   ├── requestid.go                     # Injects X-Request-ID header (UUID)
│   │   ├── auth.go                          # Virtual key validation → resolve to provider key
│   │   ├── ratelimit.go                     # Rate limit check middleware (delegates to Limiter interface)
│   │   ├── cache.go                         # Cache check middleware (delegates to Cache interface)
│   │   ├── guardrail.go                     # Guardrail middleware (delegates to Guard interface)
│   │   ├── logger.go                        # Structured request/response logging (log/slog)
│   │   └── metrics.go                       # Prometheus metric recording middleware
│   │
│   ├── cache/
│   │   ├── cache.go                         # Cache, SemanticCache, and Embedder interface definitions
│   │   ├── memory.go                        # In-memory LRU cache (sync.RWMutex + doubly linked list)
│   │   ├── redis.go                         # Redis-backed cache (go-redis client)
│   │   ├── semantic.go                      # Semantic cache: memory backend uses brute-force cosine similarity,
│   │   │                                    # Redis backend uses Redis VSS. Both satisfy SemanticCache interface.
│   │   └── embedder.go                      # Embedder implementation that calls provider /v1/embeddings endpoint
│   │
│   ├── ratelimit/
│   │   ├── limiter.go                       # Limiter interface definition
│   │   ├── memory.go                        # In-memory token bucket (sync.Mutex + atomic)
│   │   └── redis.go                         # Redis-backed sliding window (Lua script for atomicity)
│   │
│   ├── virtualkey/
│   │   ├── manager.go                       # Virtual key CRUD, mapping to provider keys, model permissions
│   │   └── store.go                         # In-memory store loaded from config (future: persistent store)
│   │
│   ├── guardrail/
│   │   ├── guard.go                         # Guard and Verdict type definitions
│   │   ├── llm.go                           # LLM-based classifier (sends content to configured LLM for evaluation)
│   │   ├── webhook.go                       # External webhook classifier (HTTP call to user's service)
│   │   └── provider.go                      # Provider-native moderation (e.g., OpenAI /v1/moderations)
│   │
│   ├── metrics/
│   │   ├── prometheus.go                    # Prometheus metric definitions (histograms, counters, gauges)
│   │   │                                    # LABEL CARDINALITY RULES (prevent Prometheus explosion):
│   │   │                                    #   SAFE labels: provider (INSTANCE name), model, status_code, method, cache_hit
│   │   │                                    #   NEVER use as labels: virtual_key, request_id, user content
│   │   │                                    #   Per-key metrics use a separate counter with hashed key dimension
│   │   └── collector.go                     # Helper functions to record metrics
│   │
│   └── pool/
│       └── pool.go                          # sync.Pool wrappers: byte buffers, bytes.Buffer, request context
│
├── configs/
│   └── gatewai.example.yaml                 # Example config with all fields annotated
│
├── test/
│   ├── integration/
│   │   ├── proxy_test.go                    # End-to-end proxy tests with mock providers
│   │   └── stream_test.go                   # SSE streaming integration tests
│   └── benchmark/
│       └── proxy_bench_test.go              # Latency and allocation benchmarks for hot path
│                                              # Load testing: use Go benchmarks (cross-platform) instead of
│                                              # shell scripts. The user is on Windows; loadtest.sh won't run locally.
│                                              # For CI, GitHub Actions runs on Ubuntu where shell scripts work.
│
├── .github/
│   └── workflows/
│       └── ci.yml                           # GitHub Actions: lint, test, benchmark, vuln scan
│
├── .golangci.yml                            # golangci-lint config (strict)
├── Dockerfile                               # Multi-stage: Go build → distroless/static-debian
│                                              # MUST copy CA certificates from builder stage:
│                                              #   COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
│                                              # Without this, ALL upstream TLS calls to OpenAI/Anthropic/Gemini FAIL
│                                              # (scratch and distroless have no CA certs)
├── Makefile                                 # make build, test, bench, lint, docker, run
├── README.md                                # Project docs, architecture diagram, quickstart, benchmarks
├── CONTRIBUTING.md                          # Contribution guidelines
├── LICENSE                                  # MIT license text
├── go.mod                                   # Go module definition
└── go.sum                                   # Go dependency checksums
```

---

## 10. Performance Techniques (EXACT patterns to use)

### 10.1 Connection Pooling (in `server.go`)

```go
// Use this EXACT transport configuration for upstream provider connections.
// This is a shared singleton — create ONCE at startup, reuse for all requests.
var upstreamTransport = &http.Transport{
    DialContext: (&net.Dialer{
        Timeout:   5 * time.Second,
        KeepAlive: 30 * time.Second,
    }).DialContext,
    MaxIdleConns:          2000,
    MaxIdleConnsPerHost:   200,
    MaxConnsPerHost:       0,               // unlimited — controlled by rate limiter
    IdleConnTimeout:       90 * time.Second,
    TLSHandshakeTimeout:   5 * time.Second,
    ExpectContinueTimeout: 1 * time.Second,
    ForceAttemptHTTP2:     true,
}
```

### 10.2 sync.Pool Buffer Reuse (in `pool/pool.go`)

```go
// ByteBufferPool recycles byte slices to avoid GC pressure under high concurrency.
var ByteBufferPool = sync.Pool{
    New: func() any {
        buf := make([]byte, 8*1024) // 8KB — chat completion request bodies are typically 1-4KB
        return &buf                  // 10k concurrent × 8KB = 80MB (within 200MB target)
    },                               // Previous 32KB × 10k = 320MB EXCEEDED the 200MB target
}

// BytesBufferPool recycles bytes.Buffer instances.
var BytesBufferPool = sync.Pool{
    New: func() any {
        return new(bytes.Buffer)
    },
}

// Usage pattern (ALWAYS follow this):
// buf := pool.BytesBufferPool.Get().(*bytes.Buffer)
// buf.Reset()  // ALWAYS reset before use
// defer pool.BytesBufferPool.Put(buf)
```

### 10.3 SSE Streaming (in `proxy/stream.go`)

```go
// StreamResponse reads SSE chunks from upstream, translates them, and flushes to client.
// Key requirements:
// 1. Use bufio.Scanner with custom split function for SSE line boundaries
// 2. Call flusher.Flush() IMMEDIATELY after each chunk write
// 3. Bind upstream context to client context (cancel upstream when client disconnects)
// 4. Use pooled buffers for reading
```

### 10.4 Context Cancellation (in `proxy/handler.go`)

```go
// ALWAYS create upstream requests with the client's context.
// When the client disconnects, this automatically cancels the upstream request,
// stopping the LLM from generating tokens we'll never deliver.
upstreamReq, err := provider.BuildRequest(r.Context(), unifiedReq, apiKey)
```

---

## 11. Phased Build Order

### Phase 1: Foundation
**Build**: `cmd/gatewai/main.go`, `internal/config/`, `internal/server/`, `internal/proxy/`, `internal/schema/`, `internal/middleware/chain.go`, `internal/middleware/bodyparser.go`, `internal/provider/provider.go`, `internal/provider/registry.go`, `internal/provider/openai/`, `internal/pool/`, `configs/gatewai.example.yaml`, `go.mod`, `Makefile`

> **Why bodyparser is in Phase 1:** the proxy handler consumes `RequestContext.ParsedRequest` — it never reads the body itself. So `chain.go` + `bodyparser.go` must exist from the start (see Section 4.1, step 0).

**Result**: A working gateway that proxies requests to OpenAI (both streaming and non-streaming). No auth, no caching, no rate limiting — just a transparent proxy with pooled buffers.

**Learning objectives**: Go project structure, HTTP reverse proxy mechanics, SSE streaming, sync.Pool, graceful shutdown.

---

### Phase 2: Multi-Provider
**Build**: `internal/provider/anthropic/`, `internal/provider/gemini/`, `internal/schema/` updates for edge cases

**Result**: Send an OpenAI-format request with `model: claude-sonnet-4-20250514` and it correctly translates to Anthropic's API format, gets a response, and translates it back to OpenAI format. Same for Gemini.

**Learning objectives**: Adapter pattern, API schema translation, SSE format differences between providers.

---

### Phase 3: Resilience & Routing
**Build**: `internal/router/`

**Result**: Requests are load-balanced across providers. If OpenAI fails, it automatically retries with Anthropic. Circuit breakers open after consecutive failures.

**Learning objectives**: Load balancing algorithms, circuit breaker state machine, exponential backoff with jitter, why jitter prevents thundering herds.

---

### Phase 4: Governance
**Build**: `internal/middleware/` (auth, ratelimit, cache — `chain.go` and `bodyparser.go` were already built in Phase 1), `internal/cache/`, `internal/ratelimit/`, `internal/virtualkey/`

**Result**: Virtual keys work, rate limiting blocks excessive requests, cache serves repeated queries instantly.

**Learning objectives**: Middleware pattern, token bucket algorithm, LRU cache eviction, cache key design, distributed rate limiting with Redis Lua scripts.

---

### Phase 5: Observability
**Build**: `internal/middleware/` (logger, metrics), `internal/metrics/`

**Result**: Prometheus metrics at `/metrics`, structured JSON logs with request traces.

**Learning objectives**: Prometheus metric types (counter, histogram, gauge), structured logging with `log/slog`, what to measure and why.

---

### Phase 6: Guardrails + Polish
**Build**: `internal/guardrail/`, `internal/middleware/guardrail.go`, `Dockerfile`, `.github/`, `.golangci.yml`, `README.md`, `CONTRIBUTING.md`, `LICENSE`, `test/`

**Result**: Content safety via pluggable classifiers. Production-ready Docker image. CI/CD pipeline. Full documentation. Benchmark results.

**Learning objectives**: Classifier-based content filtering, Docker multi-stage builds, CI/CD pipelines, open-source project hygiene, performance benchmarking.

---

## 12. Testing & Verification Strategy

### Unit Tests
```bash
go test -v -race -coverprofile=coverage.out ./...
```
- Table-driven tests for every public function
- Race detector enabled (catches concurrency bugs)
- Target: >80% coverage on core packages

### Benchmarks
```bash
go test -v -bench=. -benchmem ./internal/proxy/... ./internal/router/...
```
- `BenchmarkProxyHandler` — measure added latency and allocations per request
- `BenchmarkStreamChunkTranslation` — measure SSE translation overhead
- `BenchmarkCacheLookup` — measure cache hit latency
- `BenchmarkRateLimiterAllow` — measure rate limit check latency

### Load Test (10k concurrent)
```bash
# Using 'hey' (install: go install github.com/rakyll/hey@latest)
hey -n 10000 -c 10000 -m POST \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer gw-test-key" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"Hello"}]}' \
  http://localhost:8080/v1/chat/completions
```

### CI Pipeline (`.github/workflows/ci.yml`)
```yaml
on: [push, pull_request]
jobs:
  - golangci-lint (code quality)
  - go test -race ./... (correctness + concurrency safety)
  - go test -bench=. -benchmem (performance regression detection)
  - govulncheck ./... (dependency vulnerability scan)
```

---

## 13. Key Anti-Patterns (DO NOT DO THESE)

| Anti-Pattern | Why It's Bad | What To Do Instead |
|:---|:---|:---|
| Hardcoded provider URLs | Breaks when providers change URLs or user needs a proxy | Read from config |
| `io.ReadAll` on hot path | Allocates a new byte slice per request → GC pressure | Use `sync.Pool` buffers |
| Global mutable state | Race conditions under concurrency | Pass dependencies via constructor injection |
| Regex-based guardrails | Brittle, bypassable, unmaintainable | Pluggable classifier interface |
| `time.Sleep` for retries | Blocks goroutine, no jitter | `time.After` with exponential backoff + jitter |
| Ignoring `context.Context` | Resource leaks when clients disconnect | Always propagate context to upstream calls |
| `panic` for error handling | Crashes entire process | Return `error`, let middleware handle it |
| Unbounded caches | Memory grows indefinitely | LRU eviction with configurable max size |
| Logging secrets | API keys in logs → security breach | Redact API keys in all log output |
| Giant functions | Unmaintainable, untestable | Single responsibility, <50 lines per function |
| Reading body in multiple middlewares | Body is an `io.Reader` — can only be read once. Reading it twice panics or returns empty | Parse body ONCE in bodyparser middleware, store in context |
| Non-zero `WriteTimeout` for streaming | Go's `WriteTimeout` covers entire response lifetime. Severs streams >N seconds | Set `write_timeout: 0`, use context cancellation + upstream timeouts |
| `provider.Name` as both type and ID | Can't have two OpenAI instances with different keys | Separate `name` (instance ID) from `type` (adapter selector) |
| Failover without model mapping | Sending `gpt-4o` to Anthropic returns 404 — model doesn't exist there | Use `model_mapping` config to resolve equivalents on failover |
