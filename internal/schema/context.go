package schema

import (
	"context"
	"time"
)

// contextKey is an unexported type used as the context.Context key.
// Using a distinct type (not a plain string) prevents key collisions.
type contextKey struct{}

// WithRequestContext stores the RequestContext in a context.Context.
func WithRequestContext(ctx context.Context, rc *RequestContext) context.Context {
	return context.WithValue(ctx, contextKey{}, rc)
}

// RequestContextFrom retrieves the RequestContext stored by WithRequestContext.
// Returns nil if the context carries no RequestContext.
func RequestContextFrom(ctx context.Context) *RequestContext {
	rc, _ := ctx.Value(contextKey{}).(*RequestContext)
	return rc
}

// streamStateKey is the context key for per-stream translation state.
type streamStateKey struct{}

// WithStreamState stores a provider's per-stream SSE translation state.
// Providers whose streams need cross-chunk state (e.g. Anthropic's tool_use
// block mapping) keep it here for the duration of one streamed response.
// The proxy creates the state once per stream via the adapter.
func WithStreamState(ctx context.Context, s any) context.Context {
	return context.WithValue(ctx, streamStateKey{}, s)
}

// StreamStateFrom retrieves the stream state stored by WithStreamState.
// Returns nil if none was stored.
func StreamStateFrom(ctx context.Context) any {
	return ctx.Value(streamStateKey{})
}

// RequestContext carries metadata through the middleware chain.
// It lives in the schema package because schema imports nothing, so every
// package (middleware, proxy, router) can depend on it without circular imports.
type RequestContext struct {
	RequestID     string
	VirtualKey    string          // the virtual key used (e.g., "gw-abc123")
	ResolvedKey   string          // the actual provider API key
	Provider      string          // selected provider instance name
	Model         string          // requested model
	StartTime     time.Time       // when the request was received
	CacheHit      bool            // whether the response came from cache
	TokensIn      int             // prompt tokens (from response)
	TokensOut     int             // completion tokens (from response)
	IsStreaming   bool            // whether this is a streaming request
	ParsedRequest *UnifiedRequest // parsed request body (set by body parser middleware)
	RawBody       []byte          // raw request body bytes (for forwarding to provider)
}
