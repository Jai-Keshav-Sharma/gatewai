package schema

import "encoding/json"

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

// StreamChunk is the payload of an OpenAI-format SSE chunk (§4.2).
// It is the wire shape the client receives, shared by every adapter that
// translates foreign SSE streams into the OpenAI format.
type StreamChunk struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"` // "chat.completion.chunk"
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []StreamChoice `json:"choices"`
}

// StreamChoice is one candidate slot inside a StreamChunk.
// Delta is a map so adapters control the exact fields (role, content,
// tool_calls) without leaking nulls — Message.Content has no omitempty tag.
type StreamChoice struct {
	Index        int     `json:"index"`
	Delta        any     `json:"delta"`
	FinishReason *string `json:"finish_reason,omitempty"`
}

// MarshalSSE renders v as one SSE data line: "data: {...}\n".
// The streaming relay in the proxy appends the trailing "\n" that completes
// the "\n\n" event boundary, so adapters can safely concatenate several
// marshaled chunks into a single returned slice.
func MarshalSSE(v any) ([]byte, error) {
	payload, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	line := make([]byte, 0, len(payload)+8)
	line = append(line, "data: "...)
	line = append(line, payload...)
	return append(line, '\n'), nil
}

// DoneSSE is the terminal SSE marker: "data: [DONE]\n".
var DoneSSE = []byte("data: [DONE]\n")
