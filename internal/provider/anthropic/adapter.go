// Package anthropic implements the Anthropic (Claude) provider adapter.
// It translates between Gatewai's unified OpenAI-style format and the
// Anthropic Messages API (/v1/messages).
package anthropic

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

// anthropicVersion is required in every request to the Anthropic API.
const anthropicVersion = "2023-06-01"

// Adapter is the Anthropic wire-format translator. It is stateless and shared
// by all Anthropic instances (per-stream state lives in the stream state).
type Adapter struct{}

// Name returns the provider type identifier.
func (a *Adapter) Name() string { return "anthropic" }

// BuildRequest converts a unified request into an Anthropic Messages API
// request. Key differences from OpenAI handled here:
//   - system messages move out of messages[] into the top-level "system" field
//   - max_tokens is REQUIRED — injected from config default_max_tokens
//     when the client omits it (§7)
//   - tools use Anthropic's {name, description, input_schema} shape
//   - content is an array of typed blocks (text / image / tool_use / tool_result)
func (a *Adapter) BuildRequest(ctx context.Context, req *schema.UnifiedRequest, opts schema.BuildOptions) (*http.Request, error) {
	ar := anthropicRequest{
		Model:         req.Model,
		MaxTokens:     resolveMaxTokens(req, opts.DefaultMaxTokens),
		Stream:        req.Stream,
		Temperature:   req.Temperature,
		TopP:          req.TopP,
		StopSequences: []string(req.Stop),
		Tools:         buildTools(req.Tools),
		ToolChoice:    mapToolChoice(req.ToolChoice),
	}
	ar.Messages, ar.System = buildMessages(req.Messages)

	body, err := json.Marshal(ar)
	if err != nil {
		return nil, fmt.Errorf("anthropic: marshal request: %w", err)
	}

	endpoint := strings.TrimSuffix(opts.BaseURL, "/") + "/v1/messages"
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("anthropic: build request: %w", err)
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("x-api-key", opts.APIKey)
	hreq.Header.Set("anthropic-version", anthropicVersion)
	if req.Stream {
		hreq.Header.Set("Accept", "text/event-stream")
	}
	return hreq, nil
}

// resolveMaxTokens returns the client's max_tokens, or the instance's
// configured default. Anthropic's API rejects requests without max_tokens,
// and config validation guarantees DefaultMaxTokens > 0 for anthropic.
func resolveMaxTokens(req *schema.UnifiedRequest, defaultMaxTokens int) int {
	if req.MaxTokens != nil {
		return *req.MaxTokens
	}
	return defaultMaxTokens
}

// buildTools converts OpenAI tool definitions to Anthropic's shape.
func buildTools(tools []schema.Tool) []anthropicTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]anthropicTool, 0, len(tools))
	for _, t := range tools {
		out = append(out, anthropicTool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: t.Function.Parameters,
		})
	}
	return out
}

// mapToolChoice converts OpenAI's tool_choice into Anthropic's.
//   - "auto" -> "auto"
//   - "none" -> {"type":"none"}
//   - {"type":"function","function":{"name":X}} -> {"type":"tool","name":X}
func mapToolChoice(toolChoice any) any {
	switch tc := toolChoice.(type) {
	case nil:
		return nil
	case string:
		switch tc {
		case "auto":
			return "auto"
		case "none":
			return map[string]any{"type": "none"}
		}
	case map[string]any:
		t, _ := tc["type"].(string)
		switch t {
		case "function":
			if f, ok := tc["function"].(map[string]any); ok {
				if name, ok := f["name"].(string); ok && name != "" {
					return map[string]any{"type": "tool", "name": name}
				}
			}
		case "auto":
			return "auto"
		case "none":
			return map[string]any{"type": "none"}
		}
	}
	return nil
}

// buildMessages translates messages into Anthropic native messages and
// returns them plus the extracted system prompt.
func buildMessages(messages []schema.Message) ([]anthropicNativeMessage, string) {
	var system []string
	var out []anthropicNativeMessage
	for _, m := range messages {
		switch m.Role {
		case "system":
			if t := schema.ContentText(m.Content); t != "" {
				system = append(system, t)
			}
		case "user":
			blocks := buildContentBlocks(schema.ContentParts(m.Content))
			if len(blocks) > 0 {
				out = append(out, anthropicNativeMessage{Role: "user", Content: blocks})
			}
		case "assistant":
			blocks := buildContentBlocks(schema.ContentParts(m.Content))
			for _, tc := range m.ToolCalls {
				blocks = append(blocks, anthropicContentBlock{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Function.Name,
					Input: parseJSON(tc.Function.Arguments),
				})
			}
			if len(blocks) > 0 {
				out = append(out, anthropicNativeMessage{Role: "assistant", Content: blocks})
			}
		case "tool":
			out = append(out, anthropicNativeMessage{Role: "user", Content: []anthropicContentBlock{
				{Type: "tool_result", ToolUseID: m.ToolCallID, Content: schema.ContentText(m.Content)},
			}})
		}
	}
	return out, strings.Join(system, "\n")
}

// buildContentBlocks normalizes content parts into Anthropic blocks.
func buildContentBlocks(parts []schema.ContentPart) []anthropicContentBlock {
	blocks := make([]anthropicContentBlock, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case "text":
			blocks = append(blocks, anthropicContentBlock{Type: "text", Text: p.Text})
		case "image":
			blocks = append(blocks, anthropicContentBlock{
				Type:   "image",
				Source: &anthropicImageSource{Type: "url", URL: p.ImageURL},
			})
		}
	}
	return blocks
}

// parseJSON parses a tool-call arguments string into a JSON value, falling
// back to the raw string if it is not valid JSON.
func parseJSON(s string) any {
	if s == "" {
		return map[string]any{}
	}
	var v any
	if err := json.Unmarshal([]byte(s), &v); err == nil {
		return v
	}
	return s
}

// ParseResponse decodes a non-streaming Anthropic message into unified form.
func (a *Adapter) ParseResponse(ctx context.Context, resp *http.Response) (*schema.UnifiedResponse, error) {
	var msg anthropicMessage
	if err := json.NewDecoder(resp.Body).Decode(&msg); err != nil {
		return nil, fmt.Errorf("anthropic: decode response: %w", err)
	}

	message := &schema.Message{Role: "assistant"}
	var textParts []string
	for _, b := range msg.Content {
		switch b.Type {
		case "text":
			textParts = append(textParts, b.Text)
		case "tool_use":
			args, err := json.Marshal(b.Input)
			if err != nil {
				args = nil
			}
			message.ToolCalls = append(message.ToolCalls, schema.ToolCall{
				ID:   b.ID,
				Type: "function",
				Function: schema.FunctionCall{
					Name:      b.Name,
					Arguments: string(args),
				},
			})
		}
	}
	message.Content = strings.Join(textParts, "\n")

	ur := &schema.UnifiedResponse{
		ID:      msg.ID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   msg.Model,
		Choices: []schema.Choice{{
			Index:        0,
			Message:      message,
			FinishReason: mapFinishReason(msg.StopReason),
		}},
		Usage: &schema.Usage{
			PromptTokens:     msg.Usage.InputTokens,
			CompletionTokens: msg.Usage.OutputTokens,
			TotalTokens:      msg.Usage.InputTokens + msg.Usage.OutputTokens,
		},
	}
	return ur, nil
}

// mapFinishReason maps Anthropic stop reasons to OpenAI finish_reason values.
func mapFinishReason(reason string) *string {
	var mapped string
	switch reason {
	case "end_turn", "stop_sequence":
		mapped = "stop"
	case "max_tokens":
		mapped = "length"
	case "tool_use":
		mapped = "tool_calls"
	default:
		return nil
	}
	return &mapped
}

// SupportsStreaming reports that Anthropic supports SSE streaming.
func (a *Adapter) SupportsStreaming() bool { return true }

// --- Anthropic wire types ---

type anthropicRequest struct {
	Model         string                   `json:"model"`
	MaxTokens     int                      `json:"max_tokens"` // required by the API
	Stream        bool                     `json:"stream,omitempty"`
	System        string                   `json:"system,omitempty"`
	Messages      []anthropicNativeMessage `json:"messages"`
	Temperature   *float64                 `json:"temperature,omitempty"`
	TopP          *float64                 `json:"top_p,omitempty"`
	StopSequences []string                 `json:"stop_sequences,omitempty"`
	Tools         []anthropicTool          `json:"tools,omitempty"`
	ToolChoice    any                      `json:"tool_choice,omitempty"`
}

type anthropicNativeMessage struct {
	Role    string                  `json:"role"`
	Content []anthropicContentBlock `json:"content"`
}

type anthropicContentBlock struct {
	Type      string                `json:"type"` // text | image | tool_use | tool_result
	Text      string                `json:"text,omitempty"`
	Source    *anthropicImageSource `json:"source,omitempty"`
	ID        string                `json:"id,omitempty"` // tool_use id
	Name      string                `json:"name,omitempty"`
	Input     any                   `json:"input,omitempty"`
	ToolUseID string                `json:"tool_use_id,omitempty"`
	Content   any                   `json:"content,omitempty"` // tool_result content
}

type anthropicImageSource struct {
	Type string `json:"type"` // "url"
	URL  string `json:"url"`
}

type anthropicTool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	InputSchema any    `json:"input_schema"`
}

type anthropicMessage struct {
	ID         string                  `json:"id"`
	Model      string                  `json:"model"`
	Content    []anthropicContentBlock `json:"content"`
	StopReason string                  `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}
