package middleware

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/Jai-Keshav-Sharma/gatewai/internal/schema"
)

// statusWriter captures the HTTP status of the response, delegating every
// write to the underlying writer. It also forwards Flush() so SSE streaming
// keeps working when this middleware wraps the response writer (§4.2).
type statusWriter struct {
	http.ResponseWriter
	status int
}

// WriteHeader records the status once (net/http panics on double-write).
func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

// Write records an implicit 200 if no header was written yet.
func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// Flush forwards to the underlying writer (required for SSE).
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Logger is middleware steps 2 and 12 of the chain (§4.1): one log line when
// the request starts, one when it completes — streaming requests log
// completion after the stream has finished (§4.3). Every line carries the
// request ID so a request's full journey can be traced.
func Logger() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rc := schema.RequestContextFrom(r.Context())
			if rc == nil {
				next.ServeHTTP(w, r)
				return
			}
			slog.Info("request_start",
				"request_id", rc.RequestID,
				"method", r.Method,
				"path", r.URL.Path,
				"model", rc.Model,
				"virtual_key", rc.VirtualKey,
			)

			sw := &statusWriter{ResponseWriter: w}
			start := time.Now()
			next.ServeHTTP(sw, r)

			slog.Info("request_complete",
				"request_id", rc.RequestID,
				"status", sw.status,
				"duration_ms", time.Since(start).Milliseconds(),
				"provider", rc.Provider,
				"model", rc.Model,
				"tokens_in", rc.TokensIn,
				"tokens_out", rc.TokensOut,
				"cache_hit", rc.CacheHit,
				"streaming", rc.IsStreaming,
			)
		})
	}
}
