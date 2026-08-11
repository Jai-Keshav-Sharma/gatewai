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

// LLMGuard uses a configured LLM as the classifier (§7 example: a provider
// INSTANCE + model + prompt + threshold). The content is sent to the
// instance's /chat/completions endpoint (OpenAI wire format — the gateway's
// canonical shape) with the configured system prompt; the model must answer
// with JSON: {"safe": bool, "reason": string, "confidence": 0.0-1.0}.
// A verdict is UNSAFE when safe=false AND confidence >= threshold.
type LLMGuard struct {
	name      string
	endpoint  string // instance base URL + "/chat/completions"
	apiKey    string
	model     string
	prompt    string
	threshold float64
	timeout   time.Duration
	client    *http.Client
}

// NewLLMGuard builds the classifier for the given instance endpoint.
func NewLLMGuard(name, baseURL, apiKey, model, prompt string, threshold float64, timeout time.Duration, client *http.Client) *LLMGuard {
	return &LLMGuard{
		name:      name,
		endpoint:  strings.TrimSuffix(baseURL, "/") + "/chat/completions",
		apiKey:    apiKey,
		model:     model,
		prompt:    prompt,
		threshold: threshold,
		timeout:   timeout,
		client:    client,
	}
}

// Name returns the guard identifier.
func (g *LLMGuard) Name() string { return g.name }

// EvaluateRequest flattens the conversation and classifies it.
func (g *LLMGuard) EvaluateRequest(ctx context.Context, messages []schema.Message) (*Verdict, error) {
	var sb strings.Builder
	for _, m := range messages {
		sb.WriteString(m.Role)
		sb.WriteString(": ")
		sb.WriteString(schema.ContentText(m.Content))
		sb.WriteString("\n")
	}
	return g.classify(ctx, sb.String())
}

// EvaluateResponse classifies the response text.
func (g *LLMGuard) EvaluateResponse(ctx context.Context, content string) (*Verdict, error) {
	return g.classify(ctx, content)
}

// classify sends the text to the classifier model and converts the answer
// into a Verdict.
func (g *LLMGuard) classify(ctx context.Context, text string) (*Verdict, error) {
	body, err := json.Marshal(map[string]any{
		"model": g.model,
		"messages": []map[string]any{
			{"role": "system", "content": g.prompt},
			{"role": "user", "content": text},
		},
		"temperature": 0,
	})
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
		return nil, fmt.Errorf("llm guard: status %d", resp.StatusCode)
	}

	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if len(out.Choices) == 0 {
		return nil, fmt.Errorf("llm guard: empty response")
	}

	var answer struct {
		Safe       bool    `json:"safe"`
		Reason     string  `json:"reason"`
		Confidence float64 `json:"confidence"`
	}
	if err := json.Unmarshal([]byte(out.Choices[0].Message.Content), &answer); err != nil {
		return nil, fmt.Errorf("llm guard: classifier returned non-JSON: %q", out.Choices[0].Message.Content)
	}

	return &Verdict{
		Safe:       answer.Safe || answer.Confidence < g.threshold,
		Reason:     answer.Reason,
		Confidence: answer.Confidence,
		GuardName:  g.name,
	}, nil
}
