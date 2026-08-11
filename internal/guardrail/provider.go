package guardrail

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Jai-Keshav-Sharma/gatewai/internal/schema"
)

// ProviderGuard uses the provider's BUILT-IN moderation endpoint (§7
// example: OpenAI /v1/moderations on a provider instance). No custom rules —
// the provider's own safety model decides.
type ProviderGuard struct {
	name     string
	endpoint string // instance base URL + "/moderations"
	apiKey   string
	timeout  time.Duration
	client   *http.Client
}

// NewProviderGuard builds the classifier for the given instance endpoint.
func NewProviderGuard(name, baseURL, apiKey string, timeout time.Duration, client *http.Client) *ProviderGuard {
	return &ProviderGuard{
		name:     name,
		endpoint: strings.TrimSuffix(baseURL, "/") + "/moderations",
		apiKey:   apiKey,
		timeout:  timeout,
		client:   client,
	}
}

// Name returns the guard identifier.
func (g *ProviderGuard) Name() string { return g.name }

// EvaluateRequest flattens the conversation and runs moderation on it.
func (g *ProviderGuard) EvaluateRequest(ctx context.Context, messages []schema.Message) (*Verdict, error) {
	var text string
	for _, m := range messages {
		text += schema.ContentText(m.Content) + "\n"
	}
	return g.moderate(ctx, text)
}

// EvaluateResponse runs moderation on the response text.
func (g *ProviderGuard) EvaluateResponse(ctx context.Context, content string) (*Verdict, error) {
	return g.moderate(ctx, content)
}

func (g *ProviderGuard) moderate(ctx context.Context, text string) (*Verdict, error) {
	body, err := json.Marshal(map[string]any{"input": text})
	if err != nil {
		return nil, err
	}
	reqCtx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, g.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+g.apiKey)

	resp, err := g.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("provider guard: status %d", resp.StatusCode)
	}

	var out struct {
		Results []struct {
			Flagged bool `json:"flagged"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	flagged := len(out.Results) > 0 && out.Results[0].Flagged
	return &Verdict{
		Safe:       !flagged,
		Reason:     "flagged by provider moderation",
		Confidence: 1.0,
		GuardName:  g.name,
	}, nil
}
