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
