// Package guardrail implements pluggable content-safety classifiers (§3.1:
// NO regex, NO keyword lists, NO pattern matching — ever). Guards are
// interfaces; the classifier logic lives behind them, so rules can be
// swapped without touching the gateway core.
package guardrail

import (
	"context"

	"github.com/Jai-Keshav-Sharma/gatewai/internal/schema"
)

// Verdict represents the result of a guard evaluation (plan §5.5 — exact).
type Verdict struct {
	Safe       bool    // true if content passed the check
	Reason     string  // human-readable explanation (empty if safe)
	Confidence float64 // 0.0 to 1.0, how confident the classifier is
	GuardName  string  // which guard produced this verdict
}

// Guard evaluates content for safety/compliance (plan §5.5 — exact).
// Three implementations: LLMGuard, WebhookGuard, ProviderGuard.
type Guard interface {
	// Name returns the guard identifier (e.g., "openai-moderation", "custom-webhook").
	Name() string

	// EvaluateRequest checks the user's prompt before it's sent to a provider.
	// Return a Verdict. If Safe=false, the request is rejected with the Reason.
	EvaluateRequest(ctx context.Context, messages []schema.Message) (*Verdict, error)

	// EvaluateResponse checks the provider's response before it's returned to the client.
	// Return a Verdict. If Safe=false, the response is replaced with an error message.
	EvaluateResponse(ctx context.Context, content string) (*Verdict, error)
}
