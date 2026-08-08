// Package proxy implements the core reverse-proxy handler: it receives the
// parsed request from the middleware chain, dispatches it to a provider
// instance, and writes the translated response back to the client (§4.1).
package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/Jai-Keshav-Sharma/gatewai/internal/pool"
	"github.com/Jai-Keshav-Sharma/gatewai/internal/provider"
	"github.com/Jai-Keshav-Sharma/gatewai/internal/schema"
)

// Handler is the chat completions proxy. Dependencies arrive via the
// constructor (dependency injection — no global state), so the handler is
// stateless and safe for concurrent use by all requests.
type Handler struct {
	registry *provider.Registry
	client   *http.Client
}

// NewHandler builds the proxy handler with the shared upstream transport.
// The transport is a singleton created once at startup (see server package).
func NewHandler(reg *provider.Registry, transport *http.Transport) *Handler {
	return &Handler{
		registry: reg,
		client:   &http.Client{Transport: transport},
	}
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

	inst, ok := h.registry.Resolve(req.Model)
	if !ok {
		schema.NewInvalidRequestError(fmt.Sprintf("model %q is not configured on any provider", req.Model)).WriteJSON(w)
		return
	}
	rc.Provider = inst.Name()

	// Per-provider timeout (config), layered on top of the client context.
	// It bounds the whole upstream interaction, streaming included — this is
	// the mechanism the plan relies on instead of a server WriteTimeout.
	ctx, cancel := context.WithTimeout(r.Context(), inst.Timeout())
	defer cancel()

	// Build the upstream request with the CLIENT's context (via the derived
	// ctx above): when the client disconnects, the upstream request is
	// cancelled automatically, stopping the LLM from generating tokens we
	// will never deliver (§10.4).
	upstream, err := inst.BuildRequest(ctx, req, inst.APIKey())
	if err != nil {
		schema.NewInternalError("failed to build upstream request: " + err.Error()).WriteJSON(w)
		return
	}

	resp, err := h.client.Do(upstream)
	if err != nil {
		schema.NewProviderError(fmt.Sprintf("upstream request failed: %v", err)).WriteJSON(w)
		return
	}
	defer resp.Body.Close()

	// Phase 1 is a transparent proxy: provider errors pass through verbatim
	// (status, headers, body). Unified error translation arrives with the
	// governance and routing phases.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		h.passthrough(w, resp)
		return
	}

	if req.Stream {
		h.stream(w, resp, inst, ctx)
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
