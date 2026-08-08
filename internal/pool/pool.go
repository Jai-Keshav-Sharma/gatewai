// Package pool provides sync.Pool-backed buffers.
//
// Why sync.Pool? Allocating a new []byte per request puts constant pressure on
// the garbage collector (GC) under load. sync.Pool keeps a small stash of
// previously used buffers so that buffers get REUSED instead of re-allocated.
// Under a 10k-request burst the pool holds at most ~10k × 8KB = 80MB, well
// within the 200MB memory target (§2).
//
// RULE: never return a buffer that grew beyond its pooled size back to the
// pool — a giant buffer that stays in the pool wastes memory for everyone.
package pool

import (
	"bytes"
	"context"
	"sync"
)

// ByteBufferPool recycles byte slices to avoid GC pressure under high concurrency.
var ByteBufferPool = sync.Pool{
	New: func() any {
		buf := make([]byte, 8*1024) // 8KB — chat completion request bodies are typically 1-4KB
		return &buf                 // 10k concurrent × 8KB = 80MB (within 200MB target)
	}, // Previous 32KB × 10k = 320MB EXCEEDED the 200MB target
}

// BytesBufferPool recycles bytes.Buffer instances.
var BytesBufferPool = sync.Pool{
	New: func() any {
		return new(bytes.Buffer)
	},
}

// bodyBufferKey is the context key for the request-body buffer.
type bodyBufferKey struct{}

// WithBodyBuffer stores the bytes.Buffer backing a request's raw body in the
// context. The buffer is owned by the request for its whole lifetime (the
// RequestContext.RawBody slice aliases its memory), so it must be returned to
// the pool by the final consumer — the proxy handler — after the response has
// been written. Returning it earlier would let another request overwrite the
// body bytes mid-forward.
func WithBodyBuffer(ctx context.Context, b *bytes.Buffer) context.Context {
	return context.WithValue(ctx, bodyBufferKey{}, b)
}

// BodyBufferFrom retrieves the request-body buffer stored by WithBodyBuffer.
// Returns nil if the context carries none.
func BodyBufferFrom(ctx context.Context) *bytes.Buffer {
	b, _ := ctx.Value(bodyBufferKey{}).(*bytes.Buffer)
	return b
}
