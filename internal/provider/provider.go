// Package provider defines the Provider interface that adapts each AI
// provider's API to Gatewai's unified OpenAI-compatible format.
package provider

import (
	"context"
	"net/http"

	"github.com/Jai-Keshav-Sharma/gatewai/internal/schema"
)

// Provider adapts a specific AI provider's API to Gatewai's unified format.
// Each provider (OpenAI, Anthropic, Gemini) implements this interface.
type Provider interface {
	// Name returns the provider INSTANCE identifier (e.g., "openai-1"),
	// used in routing, logs and metrics.
	Name() string

	// BuildRequest converts a unified request into a provider-native HTTP request.
	// This includes setting the correct URL, headers, auth, and body format.
	BuildRequest(ctx context.Context, req *schema.UnifiedRequest, apiKey string) (*http.Request, error)

	// ParseResponse reads the provider's HTTP response and converts it to unified format.
	// Called only for non-streaming responses.
	ParseResponse(ctx context.Context, resp *http.Response) (*schema.UnifiedResponse, error)

	// TranslateStreamChunk converts a single provider-specific SSE data line
	// into an OpenAI-format SSE data line.
	// Returns nil if the chunk should be skipped (e.g., provider-specific metadata events).
	TranslateStreamChunk(ctx context.Context, chunk []byte) ([]byte, error)

	// Models returns the list of models this provider supports, with pricing info.
	Models() []schema.Model

	// SupportsStreaming returns true if this provider supports SSE streaming.
	SupportsStreaming() bool
}
