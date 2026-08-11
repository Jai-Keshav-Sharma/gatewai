package middleware

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"time"

	"github.com/Jai-Keshav-Sharma/gatewai/internal/schema"
)

// RequestID is middleware step 1 of the chain (§4.1): generate a request ID,
// set it on the response as X-Request-ID, and record it in the RequestContext
// so every log line for this request shares one identifier (tracing).
func RequestID() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rc := schema.RequestContextFrom(r.Context())
			if rc != nil {
				id := newRequestID()
				rc.RequestID = id
				w.Header().Set("X-Request-ID", id)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// newRequestID generates a random 32-hex-digit UUID-style identifier.
// crypto/rand is used (not math/rand) because request IDs must be
// unpredictable — they surface in logs shared with clients.
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is practically unreachable; fall back to a
		// timestamp-based id so requests keep working.
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
