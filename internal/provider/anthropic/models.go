package anthropic

import "github.com/Jai-Keshav-Sharma/gatewai/internal/schema"

// catalog is Anthropic's model list with pricing (USD per 1M tokens) and
// context windows (§3.1: pricing lives here, not scattered through logic).
var catalog = []schema.Model{
	{ID: "claude-sonnet-4-20250514", Provider: "anthropic", InputPricePer1M: 3.00, OutputPricePer1M: 15.00, ContextWindow: 200000},
	{ID: "claude-haiku-4-20250414", Provider: "anthropic", InputPricePer1M: 1.00, OutputPricePer1M: 5.00, ContextWindow: 200000},
}

// Models returns the full Anthropic catalog.
func (a *Adapter) Models() []schema.Model {
	return catalog
}
