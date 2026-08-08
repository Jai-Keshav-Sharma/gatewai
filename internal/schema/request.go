// Package schema defines the unified data model for Gatewai.
//
// This is the lowest layer of the application: it imports nothing from the
// project itself, so every other package (config, provider, middleware,
// proxy, router) can depend on it without creating import cycles.
package schema

import "encoding/json"

// UnifiedRequest is the internal representation of a chat completion request.
// It mirrors the OpenAI format since that's our canonical API.
type UnifiedRequest struct {
	Model            string          `json:"model"`
	Messages         []Message       `json:"messages"`
	Temperature      *float64        `json:"temperature,omitempty"`
	TopP             *float64        `json:"top_p,omitempty"`
	N                *int            `json:"n,omitempty"`
	Stream           bool            `json:"stream"`
	StreamOptions    *StreamOptions  `json:"stream_options,omitempty"`
	Stop             StringOrArray   `json:"stop,omitempty"`
	MaxTokens        *int            `json:"max_tokens,omitempty"`
	PresencePenalty  *float64        `json:"presence_penalty,omitempty"`
	FrequencyPenalty *float64        `json:"frequency_penalty,omitempty"`
	Tools            []Tool          `json:"tools,omitempty"`
	ToolChoice       any             `json:"tool_choice,omitempty"`
	ResponseFormat   *ResponseFormat `json:"response_format,omitempty"`
}

// Message is a single turn in the conversation.
// Content is either a plain string or a list of content parts.
type Message struct {
	Role       string     `json:"role"` // "system", "user", "assistant", "tool"
	Content    any        `json:"content"`
	Name       string     `json:"name,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// StreamOptions controls streaming-specific behavior (e.g. include_usage).
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

// StringOrArray accepts either a bare JSON string or an array of strings.
// OpenAI allows BOTH forms for the "stop" field ("stop": "END" or "stop": ["A","B"]).
// A plain []string would reject the bare-string form with a 400 — breaking
// OpenAI compatibility. Adapters treat StringOrArray as a plain []string.
type StringOrArray []string

// UnmarshalJSON implements the exact normalization rule: an array stays an array,
// a bare string becomes a single-element array.
func (s *StringOrArray) UnmarshalJSON(data []byte) error {
	var arr []string
	if err := json.Unmarshal(data, &arr); err == nil {
		*s = arr
		return nil
	}
	var single string
	if err := json.Unmarshal(data, &single); err != nil {
		return err
	}
	*s = []string{single}
	return nil
}

// Tool is a function definition exposed to the model.
type Tool struct {
	Type     string   `json:"type"` // always "function"
	Function Function `json:"function"`
}

// Function describes a callable function for tool use.
type Function struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"` // JSON Schema object
}

// ToolCall is a request from the model to invoke a function.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"` // "function"
	Function FunctionCall `json:"function"`
}

// FunctionCall holds the name and JSON arguments of a tool invocation.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON string
}

// ResponseFormat requests a structured output ("text", "json_object", "json_schema").
type ResponseFormat struct {
	Type       string `json:"type"`
	JSONSchema any    `json:"json_schema,omitempty"`
}
