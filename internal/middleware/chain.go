// Package middleware composes the request lifecycle (§4.1).
package middleware

import "net/http"

// Middleware wraps an http.Handler with additional behavior.
// It receives the next handler in the chain and returns a new handler.
type Middleware func(next http.Handler) http.Handler

// Chain composes middlewares around a base handler.
// The first middleware in the slice is the outermost (executes first on request,
// last on response). This ordering matters for correctness.
func Chain(base http.Handler, mws ...Middleware) http.Handler {
	// Apply in reverse so the first middleware in the slice wraps outermost.
	for i := len(mws) - 1; i >= 0; i-- {
		base = mws[i](base)
	}
	return base
}
