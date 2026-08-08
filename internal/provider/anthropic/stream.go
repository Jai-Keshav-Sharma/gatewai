package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	"github.com/Jai-Keshav-Sharma/gatewai/internal/schema"
)

// streamState holds the cross-chunk state of one Anthropic stream:
//   - the message id/model from the message_start event, reused in every
//     emitted chunk
//   - the mapping from Anthropic content-block indices (which span ALL block
//     types) to OpenAI tool_calls indices (which count only tool calls)
//
// The adapter is a shared singleton, so this state is per-STREAM: created by
// the proxy via NewStreamState and carried in the context.
type streamState struct {
	messageID   string
	model       string
	created     int64
	blockToTool map[int]int
	nextTool    int
}

// NewStreamState creates the per-stream state (Anthropic streams need it).
func (a *Adapter) NewStreamState() any {
	return &streamState{blockToTool: make(map[int]int)}
}

// EndOfStream returns nil — the translator emits "data: [DONE]" itself when
// it sees the message_stop event.
func (a *Adapter) EndOfStream() []byte { return nil }

// TranslateStreamChunk converts one Anthropic SSE event into OpenAI-format
// SSE line(s). Anthropic sends "event: <type>" / "data: {...}" pairs with
// blank-line separators; the event-name lines, pings and blank lines are
// skipped, and only meaningful events produce output. Each translated chunk
// is returned with a trailing "\n" so the relay's appended "\n" completes
// the "\n\n" event boundary.
func (a *Adapter) TranslateStreamChunk(ctx context.Context, chunk []byte) ([]byte, error) {
	line := bytes.TrimSpace(chunk)
	if len(line) == 0 {
		return nil, nil // blank event separator
	}
	if bytes.HasPrefix(line, []byte("event:")) || bytes.HasPrefix(line, []byte(":")) {
		return nil, nil // event name line / comment / ping
	}
	if !bytes.HasPrefix(line, []byte("data:")) {
		return nil, nil // unknown line shape — not part of the SSE contract
	}

	st, _ := schema.StreamStateFrom(ctx).(*streamState)
	if st == nil {
		return nil, nil // no state: nothing we can translate reliably
	}

	payload := bytes.TrimSpace(line[len("data:"):])
	var ev anthropicEvent
	if err := json.Unmarshal(payload, &ev); err != nil {
		return nil, nil // not JSON — skip
	}

	switch ev.Type {
	case "message_start":
		if ev.Message != nil {
			st.messageID = ev.Message.ID
			st.model = ev.Message.Model
			st.created = time.Now().Unix()
		}
		// First chunk: announce the assistant role, like OpenAI does.
		return emitChunk(st, map[string]any{"role": "assistant"}, nil)

	case "content_block_start":
		cb := ev.ContentBlock
		if cb == nil || cb.Type != "tool_use" {
			return nil, nil // text blocks produce nothing until their deltas
		}
		// A tool call begins: id + name arrive here, arguments later.
		toolIdx := st.nextTool
		st.nextTool++
		if ev.Index != nil {
			st.blockToTool[*ev.Index] = toolIdx
		}
		return emitChunk(st, map[string]any{"tool_calls": []map[string]any{{
			"index":    toolIdx,
			"id":       cb.ID,
			"type":     "function",
			"function": map[string]any{"name": cb.Name, "arguments": ""},
		}}}, nil)

	case "content_block_delta":
		d := ev.Delta
		if d == nil {
			return nil, nil
		}
		switch d.Type {
		case "text_delta":
			return emitChunk(st, map[string]any{"content": d.Text}, nil)
		case "input_json_delta":
			toolIdx := 0
			if ev.Index != nil {
				if i, ok := st.blockToTool[*ev.Index]; ok {
					toolIdx = i
				}
			}
			return emitChunk(st, map[string]any{"tool_calls": []map[string]any{{
				"index":    toolIdx,
				"function": map[string]any{"arguments": d.PartialJSON},
			}}}, nil)
		}
		return nil, nil

	case "message_delta":
		// Final chunk: carry the finish_reason (and close the delta object).
		var fr *string
		if d := ev.Delta; d != nil {
			fr = mapFinishReason(d.StopReason)
		}
		return emitChunk(st, map[string]any{}, fr)

	case "message_stop":
		// Anthropic never sends "data: [DONE]" — we synthesize it.
		return schema.DoneSSE, nil

	case "error":
		// Mid-stream upstream error: nothing safe to synthesize — drop it.
		return nil, nil

	default: // "ping", "content_block_stop", ...
		return nil, nil
	}
}

// emitChunk renders one OpenAI-format SSE data line from a delta payload,
// tagged with the stream's id/model/created.
func emitChunk(st *streamState, delta map[string]any, finishReason *string) ([]byte, error) {
	chunk := schema.StreamChunk{
		ID:      st.messageID,
		Object:  "chat.completion.chunk",
		Created: st.created,
		Model:   st.model,
		Choices: []schema.StreamChoice{{Index: 0, Delta: delta, FinishReason: finishReason}},
	}
	return schema.MarshalSSE(chunk)
}

// --- wire types for SSE events ---

type anthropicEvent struct {
	Type         string                  `json:"type"`
	Message      *anthropicStreamMessage `json:"message"`
	Index        *int                    `json:"index"`
	ContentBlock *anthropicContentBlock  `json:"content_block"`
	Delta        *anthropicStreamDelta   `json:"delta"`
}

type anthropicStreamMessage struct {
	ID    string `json:"id"`
	Model string `json:"model"`
}

type anthropicStreamDelta struct {
	Type        string `json:"type"` // text_delta | input_json_delta
	Text        string `json:"text"`
	PartialJSON string `json:"partial_json"`
	StopReason  string `json:"stop_reason"`
}
