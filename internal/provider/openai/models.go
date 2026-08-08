package openai

import "github.com/Jai-Keshav-Sharma/gatewai/internal/schema"

// catalog is OpenAI's model list with pricing (USD per 1M tokens) and
// context windows. Pricing lives here — in the adapter's data file — never
// scattered through logic (zero hardcoding policy, §3.1).
var catalog = []schema.Model{
	{ID: "gpt-4o", Provider: "openai", InputPricePer1M: 2.50, OutputPricePer1M: 10.00, ContextWindow: 128000},
	{ID: "gpt-4o-mini", Provider: "openai", InputPricePer1M: 0.15, OutputPricePer1M: 0.60, ContextWindow: 128000},
}

// Models returns the full OpenAI catalog.
func (a *Adapter) Models() []schema.Model {
	return catalog
}
