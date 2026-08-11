package middleware

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/Jai-Keshav-Sharma/gatewai/internal/cache"
	"github.com/Jai-Keshav-Sharma/gatewai/internal/guardrail"
	"github.com/Jai-Keshav-Sharma/gatewai/internal/schema"
)

// Guardrail is middleware steps 7 and 8 of the chain (§4.1):
//   - step 7 (pre-request): evaluate the prompt through the pre_request
//     classifiers before any provider call; a Safe=false verdict rejects the
//     request with 400 guardrail_blocked.
//   - step 8 (post-response): evaluate the response text before the client
//     sees it. Non-streaming responses are buffered for the check. Streaming
//     is SKIPPED by default (chunks are already sent); with buffer_mode the
//     whole stream is buffered first — which kills streaming latency, so it
//     must be explicitly enabled (§4.3).
//
// Cache hits skip this middleware entirely (steps 7-9 are skipped on a HIT,
// §4.1). Guard classifier ERRORS fail OPEN (traffic must not die because a
// classifier is unavailable) and are logged.
type Guardrail struct {
	pre        []guardrail.Guard
	post       []guardrail.Guard
	bufferMode bool
}

// NewGuardrail builds the middleware from the configured guards.
func NewGuardrail(pre, post []guardrail.Guard, bufferMode bool) *Guardrail {
	return &Guardrail{pre: pre, post: post, bufferMode: bufferMode}
}

// Middleware implements the middleware chain step.
func (m *Guardrail) Middleware() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rc := schema.RequestContextFrom(r.Context())
			if rc == nil || rc.ParsedRequest == nil {
				next.ServeHTTP(w, r)
				return
			}

			// Step 7: pre-request guardrails.
			if !m.checkRequest(w, r, rc) {
				return
			}

			// Step 8: post-response guardrails — only when there are guards
			// AND the response can still be withheld (non-streaming, or
			// streaming with buffer_mode enabled).
			if len(m.post) == 0 || (rc.IsStreaming && !m.bufferMode) {
				next.ServeHTTP(w, r)
				return
			}

			bw := &bufferWriter{ResponseWriter: w}
			next.ServeHTTP(bw, r)

			if bw.status >= 300 {
				bw.flushTo(w) // don't guard errors — pass through
				return
			}

			content := extractResponseContent(bw.buf.Bytes(), rc.IsStreaming)
			verdict, err := m.checkResponse(r, content)
			if err != nil {
				slog.Warn("post-response guard failed; allowing", "err", err)
			}
			if verdict != nil && !verdict.Safe {
				// The provider's response is withheld and the cached copy is
				// invalidated so the blocked content never reaches anyone.
				if slot := cache.ResponseSlotFrom(r.Context()); slot != nil {
					slot.Set(nil)
				}
				schema.NewGuardrailBlockedError("Content blocked by guardrail: " + verdict.Reason).WriteJSON(w)
				return
			}
			bw.flushTo(w)
		})
	}
}

// checkRequest runs the pre-request guards and writes a 400 on a block.
func (m *Guardrail) checkRequest(w http.ResponseWriter, r *http.Request, rc *schema.RequestContext) bool {
	for _, g := range m.pre {
		verdict, err := g.EvaluateRequest(r.Context(), rc.ParsedRequest.Messages)
		if err != nil {
			slog.Warn("pre-request guard failed; allowing", "guard", g.Name(), "err", err)
			continue
		}
		if !verdict.Safe {
			schema.NewGuardrailBlockedError("Content blocked by guardrail: " + verdict.Reason).WriteJSON(w)
			return false
		}
	}
	return true
}

// checkResponse runs the post-response guards; classifier errors fail open.
func (m *Guardrail) checkResponse(r *http.Request, content string) (*guardrail.Verdict, error) {
	for _, g := range m.post {
		verdict, err := g.EvaluateResponse(r.Context(), content)
		if err != nil {
			return nil, err
		}
		if !verdict.Safe {
			return verdict, nil
		}
	}
	return nil, nil
}

// extractResponseContent pulls the assistant text out of a buffered response:
// a JSON UnifiedResponse (non-streaming) or SSE data lines (streaming).
func extractResponseContent(data []byte, streaming bool) string {
	if streaming {
		acc := cache.NewAccumulator()
		for _, line := range bytes.Split(data, []byte("\n")) {
			if bytes.HasPrefix(line, []byte("data:")) && !bytes.Contains(line, []byte("[DONE]")) {
				acc.Add(line)
			}
		}
		resp := acc.Response()
		if len(resp.Choices) > 0 && resp.Choices[0].Message != nil {
			if text, ok := resp.Choices[0].Message.Content.(string); ok {
				return text
			}
		}
		return ""
	}
	var ur schema.UnifiedResponse
	if err := json.Unmarshal(data, &ur); err != nil {
		return ""
	}
	if len(ur.Choices) > 0 && ur.Choices[0].Message != nil {
		if text, ok := ur.Choices[0].Message.Content.(string); ok {
			return text
		}
	}
	return ""
}

// bufferWriter captures the response instead of sending it, so guards can
// evaluate the content first. It forwards Flush() as a no-op (streaming
// chunks are held); flushTo releases everything once the verdict is in.
type bufferWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	buf         bytes.Buffer
}

func (b *bufferWriter) WriteHeader(code int) {
	if !b.wroteHeader {
		b.wroteHeader = true
		b.status = code
	}
}

func (b *bufferWriter) Write(p []byte) (int, error) {
	if !b.wroteHeader {
		b.wroteHeader = true
		b.status = http.StatusOK
	}
	return b.buf.Write(p)
}

func (b *bufferWriter) Flush() {} // buffered — nothing reaches the client yet

func (b *bufferWriter) flushTo(w http.ResponseWriter) {
	if !b.wroteHeader {
		b.status = http.StatusOK
	}
	w.WriteHeader(b.status)
	_, _ = w.Write(b.buf.Bytes())
}
