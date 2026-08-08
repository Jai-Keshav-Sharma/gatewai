// Package openai implements the OpenAI provider adapter.
// Since Gatewai's canonical format IS the OpenAI format, this adapter is
// mostly passthrough: requests and responses are already in the right shape.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/Jai-Keshav-Sharma/gatewai/internal/schema"
)

// Adapter is the OpenAI wire-format translator. It is stateless and shared
// by all OpenAI instances.
type Adapter struct{}

// Name returns the provider type identifier.
func (a *Adapter) Name() string { return "openai" }

// BuildRequest converts a unified request into an OpenAI HTTP request.
// The endpoint is baseURL + "/chat/completions"; baseURL comes from the
// instance's config (overridable for Azure, proxies, mock servers, etc.).
func (a *Adapter) BuildRequest(ctx context.Context, req *schema.UnifiedRequest, apiKey, baseURL string) (*http.Request, error) {
	endpoint := strings.TrimSuffix(baseURL, "/") + "/chat/completions"

	// Our schema IS the OpenAI wire format, so marshaling the unified request
	// produces a byte-for-byte compatible body (including stream: true).
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("openai: marshal request: %w", err)
	}

	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("openai: build request: %w", err)
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Authorization", "Bearer "+apiKey)
	if req.Stream {
		hreq.Header.Set("Accept", "text/event-stream")
	}
	return hreq, nil
}

// ParseResponse decodes an OpenAI chat completion response into unified form.
func (a *Adapter) ParseResponse(ctx context.Context, resp *http.Response) (*schema.UnifiedResponse, error) {
	var ur schema.UnifiedResponse
	if err := json.NewDecoder(resp.Body).Decode(&ur); err != nil {
		return nil, fmt.Errorf("openai: decode response: %w", err)
	}
	return &ur, nil
}

// SupportsStreaming reports that OpenAI supports SSE streaming.
func (a *Adapter) SupportsStreaming() bool { return true }
