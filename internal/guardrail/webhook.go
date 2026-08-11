package guardrail

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/Jai-Keshav-Sharma/gatewai/internal/schema"
)

// WebhookGuard calls an external classifier service (§7 example: a user-run
// service with a /classify endpoint). The gateway POSTs the content and the
// service answers with the same verdict shape as the LLM guard:
// {"safe": bool, "reason": string, "confidence": 0.0-1.0}.
type WebhookGuard struct {
	name    string
	url     string
	timeout time.Duration
	client  *http.Client
}

// NewWebhookGuard builds the classifier for the given endpoint.
func NewWebhookGuard(name, url string, timeout time.Duration, client *http.Client) *WebhookGuard {
	return &WebhookGuard{name: name, url: url, timeout: timeout, client: client}
}

// Name returns the guard identifier.
func (g *WebhookGuard) Name() string { return g.name }

// EvaluateRequest flattens the conversation and calls the webhook.
func (g *WebhookGuard) EvaluateRequest(ctx context.Context, messages []schema.Message) (*Verdict, error) {
	var text string
	for _, m := range messages {
		text += schema.ContentText(m.Content) + "\n"
	}
	return g.classify(ctx, text)
}

// EvaluateResponse classifies the response text.
func (g *WebhookGuard) EvaluateResponse(ctx context.Context, content string) (*Verdict, error) {
	return g.classify(ctx, content)
}

func (g *WebhookGuard) classify(ctx context.Context, text string) (*Verdict, error) {
	body, err := json.Marshal(map[string]any{"content": text})
	if err != nil {
		return nil, err
	}
	reqCtx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, g.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("webhook guard: status %d", resp.StatusCode)
	}

	var answer struct {
		Safe       bool    `json:"safe"`
		Reason     string  `json:"reason"`
		Confidence float64 `json:"confidence"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
		return nil, err
	}
	return &Verdict{
		Safe:       answer.Safe,
		Reason:     answer.Reason,
		Confidence: answer.Confidence,
		GuardName:  g.name,
	}, nil
}
