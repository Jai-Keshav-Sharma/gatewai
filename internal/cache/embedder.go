package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// HTTPEmbedder is an Embedder that calls a provider's /v1/embeddings
// endpoint (the OpenAI-compatible embeddings API) on the configured
// instance's base URL with that instance's key (§7 cache.semantic:
// embedding_provider is an INSTANCE name — keys live on instances).
type HTTPEmbedder struct {
	baseURL string
	apiKey  string
	model   string
	client  *http.Client
}

// NewHTTPEmbedder builds an embedder for the given instance endpoint.
func NewHTTPEmbedder(baseURL, apiKey, model string, client *http.Client) *HTTPEmbedder {
	return &HTTPEmbedder{baseURL: baseURL, apiKey: apiKey, model: model, client: client}
}

// Embed converts text into a vector embedding via POST /v1/embeddings.
func (e *HTTPEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	body, err := json.Marshal(map[string]any{
		"model": e.model,
		"input": text,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimSuffix(e.baseURL, "/")+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.apiKey)

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embed: status %d", resp.StatusCode)
	}
	var out struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("embed: decode: %w", err)
	}
	if len(out.Data) == 0 {
		return nil, fmt.Errorf("embed: empty response")
	}
	return out.Data[0].Embedding, nil
}
