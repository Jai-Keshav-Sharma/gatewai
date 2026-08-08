package middleware

import (
	"bytes"
	"encoding/json"
	"net/http"
	"time"

	"github.com/Jai-Keshav-Sharma/gatewai/internal/pool"
	"github.com/Jai-Keshav-Sharma/gatewai/internal/schema"
)

// BodyParser is step 0 of the middleware chain (§4.1): read the request body
// ONCE into a pooled buffer, parse it into a UnifiedRequest, and store both
// in the RequestContext. Every downstream middleware consumes the parsed
// request from the context — the body is NEVER read again.
//
// An http.Request body is an io.Reader: it can be consumed exactly once.
// Reading it in multiple middlewares would return empty bytes the second time
// — or panic. This single, early parse is the fix.
func BodyParser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only chat completions carry a JSON body we need to parse.
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			next.ServeHTTP(w, r)
			return
		}

		buf := pool.BytesBufferPool.Get().(*bytes.Buffer)
		buf.Reset()

		if _, err := buf.ReadFrom(r.Body); err != nil {
			pool.BytesBufferPool.Put(buf)
			schema.NewInternalError("failed to read request body").WriteJSON(w)
			return
		}

		if buf.Len() == 0 {
			pool.BytesBufferPool.Put(buf)
			schema.NewInvalidRequestError("request body must not be empty").WriteJSON(w)
			return
		}

		var req schema.UnifiedRequest
		if err := json.Unmarshal(buf.Bytes(), &req); err != nil {
			pool.BytesBufferPool.Put(buf)
			schema.NewInvalidRequestError("invalid JSON in request body: " + err.Error()).WriteJSON(w)
			return
		}

		rc := &schema.RequestContext{
			ParsedRequest: &req,
			RawBody:       buf.Bytes(), // aliases the buffer — valid until the proxy returns it
			StartTime:     time.Now(),
			IsStreaming:   req.Stream,
			Model:         req.Model,
		}
		ctx := schema.WithRequestContext(r.Context(), rc)
		ctx = pool.WithBodyBuffer(ctx, buf) // the proxy returns the buffer to the pool when done
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
