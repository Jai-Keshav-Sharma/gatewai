// Package proxy implements the core reverse-proxy handler: it receives the
// parsed request from the middleware chain, dispatches it through the router
// (which owns retries, failover and circuit breakers), and writes the
// translated response back to the client (§4.1).
package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/Jai-Keshav-Sharma/gatewai/internal/pool"
	"github.com/Jai-Keshav-Sharma/gatewai/internal/router"
	"github.com/Jai-Keshav-Sharma/gatewai/internal/schema"
)

// Handler is the chat completions proxy. Dependencies arrive via the
// constructor (dependency injection — no global state), so the handler is
// stateless and safe for concurrent use by all requests.
type Handler struct {
	router *router.Router
}

// NewHandler builds the proxy handler. Dispatch — including the HTTP client
// and upstream transport — lives in the router.
func NewHandler(r *router.Router) *Handler {
	return &Handler{router: r}
}

// ServeHTTP handles a single /v1/chat/completions request.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The body parser owns a pooled buffer backing RawBody; we are the final
	// consumer, so we return it to the pool once the response is done.
	if b := pool.BodyBufferFrom(r.Context()); b != nil {
		defer pool.BytesBufferPool.Put(b)
	}

	rc := schema.RequestContextFrom(r.Context())
	if rc == nil || rc.ParsedRequest == nil {
		// Body parser runs before us in the chain, so this is unreachable —
		// kept as a defensive invariant check.
		schema.NewInternalError("missing request context").WriteJSON(w)
		return
	}
	req := rc.ParsedRequest

	res, err := h.router.Dispatch(r.Context(), req)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return // client gone — nothing to write, nothing to retry
		}
		var ge *schema.GatewaiError
		if errors.As(err, &ge) {
			ge.WriteJSON(w)
			return
		}
		schema.NewInternalError(err.Error()).WriteJSON(w)
		return
	}

	inst := res.Instance
	rc.Provider = inst.Name()

	resp := res.Resp
	defer resp.Body.Close()

	// A non-retryable 4xx passes through verbatim (the router already
	// decided not to retry or fail over a client error).
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		h.passthrough(w, resp)
		return
	}

	if req.Stream {
		h.stream(w, resp, inst, r.Context())
		return
	}

	ur, err := inst.ParseResponse(r.Context(), resp)
	if err != nil {
		schema.NewInternalError("failed to parse provider response: " + err.Error()).WriteJSON(w)
		return
	}
	if ur.Model == "" {
		// Some providers (Gemini) don't echo the model name; the request's
		// model is the truthful answer.
		ur.Model = req.Model
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(ur); err != nil {
		// Client disconnected mid-write; nothing more we can do.
		return
	}
}

// passthrough forwards a non-2xx upstream response to the client unchanged.
// Content-Length is skipped so net/http manages framing itself.
func (h *Handler) passthrough(w http.ResponseWriter, resp *http.Response) {
	for k, vs := range resp.Header {
		if k == "Content-Length" {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
