package gemini

import "github.com/Jai-Keshav-Sharma/gatewai/internal/schema"

// catalog is Gemini's model list with pricing (USD per 1M tokens) and
// context windows (§3.1: pricing lives here, not scattered through logic).
var catalog = []schema.Model{
	{ID: "gemini-2.5-pro", Provider: "gemini", InputPricePer1M: 1.25, OutputPricePer1M: 10.00, ContextWindow: 1000000},
	{ID: "gemini-2.5-flash", Provider: "gemini", InputPricePer1M: 0.30, OutputPricePer1M: 2.50, ContextWindow: 1000000},
}

// Models returns the full Gemini catalog.
func (a *Adapter) Models() []schema.Model {
	return catalog
}
