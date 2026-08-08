package schema

// UnifiedResponse is the internal representation of a chat completion response.
type UnifiedResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"` // "chat.completion"
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   *Usage   `json:"usage,omitempty"`
}

// Choice is a single candidate completion.
type Choice struct {
	Index        int      `json:"index"`
	Message      *Message `json:"message,omitempty"`       // non-streaming
	Delta        *Message `json:"delta,omitempty"`         // streaming
	FinishReason *string  `json:"finish_reason,omitempty"` // "stop", "length", "tool_calls", null
}

// Usage reports token counts for billing and rate limiting.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}
