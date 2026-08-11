package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Jai-Keshav-Sharma/gatewai/internal/schema"
)

// streamState tracks the tool_calls index across chunks (Gemini function
// call parts carry no index of their own), the synthesized chunk id, and
// token usage from the final chunk's usageMetadata (§4.1 step 10).
type streamState struct {
	nextTool int
	id       string
	created  int64
	usage    *schema.Usage
}

// Usage implements schema.UsageSource (read by the proxy after the stream).
func (s *streamState) Usage() *schema.Usage { return s.usage }

// NewStreamState creates the per-stream state (needed for tool call indices).
func (a *Adapter) NewStreamState() any {
	return &streamState{id: fmt.Sprintf("gemini-%d", time.Now().UnixNano()), created: time.Now().Unix()}
}

// EndOfStream returns the terminal marker: Gemini's alt=sse streams never
// send "data: [DONE]" — the connection just closes. The relay writes this
// marker after a clean EOF, so the client sees a well-formed OpenAI stream.
func (a *Adapter) EndOfStream() []byte { return schema.DoneSSE }

// TranslateStreamChunk converts one Gemini SSE data line into OpenAI-format
// SSE line(s). Gemini sends bare "data: {GenerateContentResponse}" lines with
// blank-line separators; each may carry text parts, function call parts and,
// in the final chunk, a finishReason. A single line can produce several
// OpenAI chunks, so they are concatenated (each self-terminated) in one slice.
func (a *Adapter) TranslateStreamChunk(ctx context.Context, chunk []byte) ([]byte, error) {
	line := bytes.TrimSpace(chunk)
	if len(line) == 0 {
		return nil, nil // blank separator
	}
	if bytes.HasPrefix(line, []byte(":")) {
		return nil, nil // comment / keepalive
	}
	if !bytes.HasPrefix(line, []byte("data:")) {
		return nil, nil // unknown line shape
	}

	st, _ := schema.StreamStateFrom(ctx).(*streamState)
	if st == nil {
		return nil, nil
	}

	payload := bytes.TrimSpace(line[len("data:"):])
	var gr geminiResponse
	if err := json.Unmarshal(payload, &gr); err != nil {
		return nil, nil // not JSON — skip
	}
	if gr.UsageMetadata != nil {
		st.usage = &schema.Usage{
			PromptTokens:     gr.UsageMetadata.PromptTokenCount,
			CompletionTokens: gr.UsageMetadata.CandidatesTokenCount,
			TotalTokens:      gr.UsageMetadata.TotalTokenCount,
		}
	}
	if len(gr.Candidates) == 0 {
		return nil, nil // e.g. usageMetadata-only chunk — captured, nothing to emit
	}
	cand := gr.Candidates[0]

	var out []byte
	if cand.Content != nil {
		for _, p := range cand.Content.Parts {
			switch {
			case p.Text != "":
				out = append(out, marshalChunk(st, map[string]any{"content": p.Text}, nil)...)
			case p.FunctionCall != nil:
				args, err := json.Marshal(p.FunctionCall.Args)
				if err != nil {
					args = []byte("{}")
				}
				idx := st.nextTool
				st.nextTool++
				out = append(out, marshalChunk(st, map[string]any{"tool_calls": []map[string]any{{
					"index":    idx,
					"id":       "call_" + p.FunctionCall.Name,
					"type":     "function",
					"function": map[string]any{"name": p.FunctionCall.Name, "arguments": string(args)},
				}}}, nil)...)
			}
		}
	}
	if cand.FinishReason != "" {
		out = append(out, marshalChunk(st, map[string]any{}, mapGeminiFinish(cand.FinishReason))...)
	}
	return out, nil
}

// marshalChunk renders one OpenAI-format SSE data line.
func marshalChunk(st *streamState, delta map[string]any, finishReason *string) []byte {
	chunk := schema.StreamChunk{
		ID:      st.id,
		Object:  "chat.completion.chunk",
		Created: st.created,
		Model:   "",
		Choices: []schema.StreamChoice{{Index: 0, Delta: delta, FinishReason: finishReason}},
	}
	b, err := schema.MarshalSSE(chunk)
	if err != nil {
		return nil
	}
	return b
}
