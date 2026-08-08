package openai

import "context"

// TranslateStreamChunk handles OpenAI SSE lines.
// OpenAI's SSE format IS our canonical format, so this is pure passthrough —
// every line (including "data: [DONE]") is forwarded unchanged.
// (Compare: the Anthropic and Gemini adapters must translate event shapes.)
func (a *Adapter) TranslateStreamChunk(ctx context.Context, chunk []byte) ([]byte, error) {
	return chunk, nil
}
