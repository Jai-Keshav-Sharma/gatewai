package openai

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/Jai-Keshav-Sharma/gatewai/internal/schema"
)

// streamState holds the per-stream usage captured from the final
// include_usage chunk. OpenAI translation is otherwise pure passthrough.
type streamState struct {
	usage *schema.Usage
}

// Usage implements schema.UsageSource (read by the proxy after the stream).
func (s *streamState) Usage() *schema.Usage { return s.usage }

// NewStreamState creates the per-stream state (needed for usage capture).
func (a *Adapter) NewStreamState() any { return &streamState{} }

// EndOfStream returns nil — OpenAI sends its own "data: [DONE]" marker.
func (a *Adapter) EndOfStream() []byte { return nil }

// TranslateStreamChunk handles OpenAI SSE lines.
// OpenAI's SSE format IS our canonical format, so this is pure passthrough —
// every line (including "data: [DONE]") is forwarded unchanged — EXCEPT the
// usage chunk that WE injected via include_usage: it is captured for metrics
// and stripped from the client stream. If the client originally requested
// include_usage, the chunk is forwarded as-is (§4.1 step 10 STRIPPING RULE).
func (a *Adapter) TranslateStreamChunk(ctx context.Context, chunk []byte) ([]byte, error) {
	if !bytes.Contains(chunk, []byte(`"usage"`)) {
		return chunk, nil // fast path: no usage in this line
	}

	st, _ := schema.StreamStateFrom(ctx).(*streamState)
	if st == nil {
		return chunk, nil
	}

	var parsed schema.StreamChunk
	if err := json.Unmarshal(trimDataPrefix(chunk), &parsed); err != nil {
		return chunk, nil // not a usage chunk after all — forward verbatim
	}
	if parsed.Usage == nil {
		return chunk, nil
	}
	st.usage = parsed.Usage

	// Did the client ask for usage? Then keep the chunk; otherwise strip it.
	rc := schema.RequestContextFrom(ctx)
	if rc != nil && rc.ParsedRequest != nil && rc.ParsedRequest.StreamOptions != nil && rc.ParsedRequest.StreamOptions.IncludeUsage {
		return chunk, nil
	}
	return nil, nil
}

// trimDataPrefix strips a leading "data:" from an SSE line.
func trimDataPrefix(line []byte) []byte {
	if bytes.HasPrefix(line, []byte("data:")) {
		line = line[len("data:"):]
	}
	return bytes.TrimSpace(line)
}
