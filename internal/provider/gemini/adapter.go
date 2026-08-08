// Package gemini implements the Google Gemini provider adapter.
// It translates between Gatewai's unified OpenAI-style format and the
// Gemini generateContent API (/v1beta/models/{model}:generateContent).
package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Jai-Keshav-Sharma/gatewai/internal/schema"
)

// Adapter is the Gemini wire-format translator. It is stateless and shared
// by all Gemini instances (per-stream state lives in the stream state).
type Adapter struct{}

// Name returns the provider type identifier.
func (a *Adapter) Name() string { return "gemini" }

// BuildRequest converts a unified request into a Gemini generateContent
// request. Key differences from OpenAI handled here:
//   - messages become "contents" with roles user/model/function
//   - system messages move to the top-level "systemInstruction"
//   - generation knobs live in "generationConfig" (camelCase)
//   - tools are "functionDeclarations"; tool_choice becomes functionCallingConfig
func (a *Adapter) BuildRequest(ctx context.Context, req *schema.UnifiedRequest, opts schema.BuildOptions) (*http.Request, error) {
	gr := geminiRequest{
		Contents:         buildContents(req.Messages),
		GenerationConfig: buildGenerationConfig(req),
		Tools:            buildTools(req.Tools),
	}
	if sys := systemPrompt(req.Messages); sys != "" {
		gr.SystemInstruction = &geminiPartContainer{Parts: []geminiPart{{Text: sys}}}
	}

	body, err := json.Marshal(gr)
	if err != nil {
		return nil, fmt.Errorf("gemini: marshal request: %w", err)
	}

	method := ":generateContent"
	if req.Stream {
		method = ":streamGenerateContent?alt=sse"
	}
	endpoint := strings.TrimSuffix(opts.BaseURL, "/") + "/v1beta/models/" + url.PathEscape(req.Model) + method

	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("gemini: build request: %w", err)
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("x-goog-api-key", opts.APIKey)
	if req.Stream {
		hreq.Header.Set("Accept", "text/event-stream")
	}
	return hreq, nil
}

// buildContents translates messages into Gemini "contents".
// Tool messages become role "function" with a functionResponse part; the
// function name is resolved from the preceding assistant message's tool_calls.
func buildContents(messages []schema.Message) []geminiContent {
	var contents []geminiContent
	nameByCallID := make(map[string]string)
	for _, m := range messages {
		switch m.Role {
		case "system":
			// handled separately as systemInstruction
		case "user":
			contents = append(contents, geminiContent{Role: "user", Parts: partsFromContent(m.Content)})
		case "assistant":
			parts := partsFromContent(m.Content)
			for _, tc := range m.ToolCalls {
				nameByCallID[tc.ID] = tc.Function.Name
				parts = append(parts, geminiPart{
					FunctionCall: &geminiFunctionCall{Name: tc.Function.Name, Args: parseJSON(tc.Function.Arguments)},
				})
			}
			contents = append(contents, geminiContent{Role: "model", Parts: parts})
		case "tool":
			name := nameByCallID[m.ToolCallID]
			if name == "" {
				name = m.ToolCallID // best effort
			}
			contents = append(contents, geminiContent{Role: "function", Parts: []geminiPart{{
				FunctionResponse: &geminiFunctionResponse{Name: name, Response: parseJSON(schema.ContentText(m.Content))},
			}}})
		}
	}
	return contents
}

// partsFromContent converts unified content parts into Gemini parts.
func partsFromContent(content any) []geminiPart {
	var parts []geminiPart
	for _, p := range schema.ContentParts(content) {
		switch p.Type {
		case "text":
			parts = append(parts, geminiPart{Text: p.Text})
		case "image":
			parts = append(parts, geminiPart{FileData: &geminiFileData{
				FileURI: p.ImageURL, MIMEType: "image/jpeg",
			}})
		}
	}
	return parts
}

// systemPrompt extracts and joins all system messages.
func systemPrompt(messages []schema.Message) string {
	var parts []string
	for _, m := range messages {
		if m.Role == "system" {
			if t := schema.ContentText(m.Content); t != "" {
				parts = append(parts, t)
			}
		}
	}
	return strings.Join(parts, "\n")
}

// buildGenerationConfig maps OpenAI generation knobs to Gemini's config.
func buildGenerationConfig(req *schema.UnifiedRequest) geminiGenerationConfig {
	gc := geminiGenerationConfig{
		Temperature:     req.Temperature,
		TopP:            req.TopP,
		MaxOutputTokens: req.MaxTokens,
		StopSequences:   []string(req.Stop),
	}
	if tc := mapToolChoice(req.ToolChoice); tc != nil {
		gc.ToolConfig = &geminiToolConfig{FunctionCallingConfig: *tc}
	}
	return gc
}

// mapToolChoice converts OpenAI's tool_choice into Gemini's
// functionCallingConfig: "auto" -> AUTO, "none" -> NONE,
// {"type":"function",...} -> ANY + allowedFunctionNames.
func mapToolChoice(toolChoice any) *geminiFunctionCallingConfig {
	switch tc := toolChoice.(type) {
	case nil:
		return nil
	case string:
		switch tc {
		case "auto":
			return &geminiFunctionCallingConfig{Mode: "AUTO"}
		case "none":
			return &geminiFunctionCallingConfig{Mode: "NONE"}
		}
	case map[string]any:
		t, _ := tc["type"].(string)
		switch t {
		case "function":
			var names []string
			if f, ok := tc["function"].(map[string]any); ok {
				if name, ok := f["name"].(string); ok && name != "" {
					names = []string{name}
				}
			}
			return &geminiFunctionCallingConfig{Mode: "ANY", AllowedFunctionNames: names}
		case "auto":
			return &geminiFunctionCallingConfig{Mode: "AUTO"}
		case "none":
			return &geminiFunctionCallingConfig{Mode: "NONE"}
		}
	}
	return nil
}

// buildTools converts OpenAI tool definitions into functionDeclarations.
func buildTools(tools []schema.Tool) []geminiTool {
	if len(tools) == 0 {
		return nil
	}
	decls := make([]geminiFunctionDeclaration, 0, len(tools))
	for _, t := range tools {
		decls = append(decls, geminiFunctionDeclaration{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  t.Function.Parameters,
		})
	}
	return []geminiTool{{FunctionDeclarations: decls}}
}

// parseJSON parses a JSON string into a value, falling back to the raw string.
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

// ParseResponse decodes a non-streaming Gemini response into unified form.
func (a *Adapter) ParseResponse(ctx context.Context, resp *http.Response) (*schema.UnifiedResponse, error) {
	var gr geminiResponse
	if err := json.NewDecoder(resp.Body).Decode(&gr); err != nil {
		return nil, fmt.Errorf("gemini: decode response: %w", err)
	}

	ur := &schema.UnifiedResponse{
		ID:      fmt.Sprintf("gemini-%d", time.Now().UnixNano()),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   "",
	}
	for i, cand := range gr.Candidates {
		message := &schema.Message{Role: "assistant"}
		var textParts []string
		if cand.Content != nil {
			for _, p := range cand.Content.Parts {
				if p.Text != "" {
					textParts = append(textParts, p.Text)
				}
				if p.FunctionCall != nil {
					args, err := json.Marshal(p.FunctionCall.Args)
					if err != nil {
						args = nil
					}
					message.ToolCalls = append(message.ToolCalls, schema.ToolCall{
						ID:   fmt.Sprintf("call_%d", i),
						Type: "function",
						Function: schema.FunctionCall{
							Name:      p.FunctionCall.Name,
							Arguments: string(args),
						},
					})
				}
			}
		}
		message.Content = strings.Join(textParts, "\n")
		ur.Choices = append(ur.Choices, schema.Choice{
			Index:        i,
			Message:      message,
			FinishReason: mapGeminiFinish(cand.FinishReason),
		})
	}
	if gr.UsageMetadata != nil {
		ur.Usage = &schema.Usage{
			PromptTokens:     gr.UsageMetadata.PromptTokenCount,
			CompletionTokens: gr.UsageMetadata.CandidatesTokenCount,
			TotalTokens:      gr.UsageMetadata.TotalTokenCount,
		}
	}
	return ur, nil
}

// mapGeminiFinish maps Gemini finish reasons to OpenAI finish_reason values.
func mapGeminiFinish(reason string) *string {
	var mapped string
	switch reason {
	case "STOP":
		mapped = "stop"
	case "MAX_TOKENS":
		mapped = "length"
	default:
		return nil
	}
	return &mapped
}

// SupportsStreaming reports that Gemini supports SSE streaming.
func (a *Adapter) SupportsStreaming() bool { return true }

// --- Gemini wire types ---

type geminiRequest struct {
	Contents          []geminiContent        `json:"contents"`
	SystemInstruction *geminiPartContainer   `json:"systemInstruction,omitempty"`
	GenerationConfig  geminiGenerationConfig `json:"generationConfig"`
	Tools             []geminiTool           `json:"tools,omitempty"`
}

type geminiPartContainer struct {
	Parts []geminiPart `json:"parts"`
}

type geminiContent struct {
	Role  string       `json:"role"` // user | model | function
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text             string                  `json:"text,omitempty"`
	FunctionCall     *geminiFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *geminiFunctionResponse `json:"functionResponse,omitempty"`
	FileData         *geminiFileData         `json:"fileData,omitempty"`
}

type geminiFunctionCall struct {
	Name string `json:"name"`
	Args any    `json:"args"`
}

type geminiFunctionResponse struct {
	Name     string `json:"name"`
	Response any    `json:"response"`
}

type geminiFileData struct {
	MIMEType string `json:"mimeType,omitempty"`
	FileURI  string `json:"fileUri"`
}

type geminiTool struct {
	FunctionDeclarations []geminiFunctionDeclaration `json:"functionDeclarations"`
}

type geminiFunctionDeclaration struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
}

type geminiGenerationConfig struct {
	Temperature     *float64          `json:"temperature,omitempty"`
	TopP            *float64          `json:"topP,omitempty"`
	MaxOutputTokens *int              `json:"maxOutputTokens,omitempty"`
	StopSequences   []string          `json:"stopSequences,omitempty"`
	ToolConfig      *geminiToolConfig `json:"toolConfig,omitempty"`
}

type geminiToolConfig struct {
	FunctionCallingConfig geminiFunctionCallingConfig `json:"functionCallingConfig"`
}

type geminiFunctionCallingConfig struct {
	Mode                 string   `json:"mode"` // AUTO | ANY | NONE
	AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
}

type geminiResponse struct {
	Candidates []struct {
		Content      *geminiContent `json:"content"`
		FinishReason string         `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata *struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
		TotalTokenCount      int `json:"totalTokenCount"`
	} `json:"usageMetadata"`
}
