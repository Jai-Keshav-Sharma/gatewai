package proxy

import (
	"bufio"
	"context"
	"net/http"

	"github.com/Jai-Keshav-Sharma/gatewai/internal/cache"
	"github.com/Jai-Keshav-Sharma/gatewai/internal/pool"
	"github.com/Jai-Keshav-Sharma/gatewai/internal/provider"
	"github.com/Jai-Keshav-Sharma/gatewai/internal/schema"
)

// maxSSELine caps a single SSE line. Chunk JSON lines are small, but tool-call
// arguments and provider quirks can push them well past 8KB, so we allow up to
// 1MB per line and let the scanner grow internally beyond the pooled 8KB.
const maxSSELine = 1 << 20

// stream relays a provider SSE stream to the client, translating every line
// through the provider adapter (§4.2).
//
// The contract:
//  1. Each provider chunk is translated and written immediately.
//  2. http.Flusher.Flush() runs after EVERY chunk — without it, buffering
//     proxies and net/http would batch chunks and kill streaming latency.
//  3. The upstream request was built with the client context, so a client
//     disconnect cancels the upstream stream automatically.
func (h *Handler) stream(w http.ResponseWriter, resp *http.Response, inst *provider.Instance, ctx context.Context) {
	fl, ok := w.(http.Flusher)
	if !ok {
		// Unreachable with net/http's ResponseWriter — defensive check.
		schema.NewInternalError("streaming not supported by the response writer").WriteJSON(w)
		return
	}

	// Exact SSE headers from the plan (§4.2).
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fl.Flush()

	scanner := bufio.NewScanner(resp.Body)
	buf := pool.ByteBufferPool.Get().(*[]byte)
	scanner.Buffer(*buf, maxSSELine)
	defer pool.ByteBufferPool.Put(buf)

	// Per-stream translation state for stateful adapters (Anthropic tool_use
	// mapping, Gemini tool indices). Stateless adapters (OpenAI) get nil.
	ctx = schema.WithStreamState(ctx, inst.NewStreamState())

	// If caching is enabled, the cache middleware attached an accumulator:
	// tee every translated chunk into it so the completed stream can be
	// stored after [DONE] (§4.3). The reconstructed response is published
	// via cache.WithStoredResponse for the middleware to pick up.
	acc := cache.AccumulatorFrom(ctx)

	// Read the upstream stream line by line. ScanLines yields each line
	// without its trailing "\n" (and strips a trailing "\r"), so we re-add
	// the newline after every forwarded line. Blank lines become "\n", which
	// preserves the SSE event boundaries exactly ("data: ...\n\n").
	for scanner.Scan() {
		line := scanner.Bytes()

		translated, err := inst.TranslateStreamChunk(ctx, line)
		if err != nil {
			// Translation failure mid-stream: bytes are already flowing to the
			// client, so there is nothing safe to send. Abort silently.
			return
		}
		if translated == nil {
			continue // adapter says skip this line (provider metadata)
		}

		if _, err := w.Write(translated); err != nil {
			return // client went away — upstream is canceled via the context
		}
		if _, err := w.Write(newline); err != nil {
			return
		}
		if acc != nil {
			acc.Add(translated) // tee for caching
		}
		fl.Flush()
	}

	// scanner.Err() is nil on a CLEAN EOF. Providers that never send their own
	// terminal marker (Gemini) get one synthesized here so the client always
	// sees a well-formed OpenAI stream ("data: [DONE]\n\n"). On an aborted
	// stream (upstream break or client disconnect) chunks are already
	// delivered; nothing recoverable remains, so we just stop.
	if err := scanner.Err(); err == nil {
		if marker := inst.EndOfStream(); marker != nil {
			if _, err := w.Write(marker); err == nil {
				fl.Flush()
			}
		}
	}

	// On a clean stream: capture the per-provider usage the adapter
	// extracted from its events (§4.1 step 10 — OpenAI include_usage chunk /
	// Anthropic message_start+message_delta / Gemini usageMetadata), record
	// it on the request (metrics + TPM post-charge), and attach it to the
	// reconstructed cached response so cache hits are charged too.
	if scanner.Err() == nil {
		var usage *schema.Usage
		if us, ok := schema.StreamStateFrom(ctx).(schema.UsageSource); ok {
			usage = us.Usage()
		}
		if usage != nil {
			recordTokens(schema.RequestContextFrom(ctx), usage)
		}
		if acc != nil {
			ur := acc.Response()
			if usage != nil {
				ur.Usage = usage
			}
			if slot := cache.ResponseSlotFrom(ctx); slot != nil {
				slot.Set(ur)
			}
		}
	}
}

// newline avoids re-allocating a 1-byte slice per chunk.
var newline = []byte("\n")
