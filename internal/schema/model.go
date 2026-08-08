package schema

// Model describes a model supported by a provider.
type Model struct {
	ID               string  `json:"id"`                  // e.g., "gpt-4o"
	Provider         string  `json:"provider"`            // e.g., "openai"
	InputPricePer1M  float64 `json:"input_price_per_1m"`  // USD per 1M input tokens
	OutputPricePer1M float64 `json:"output_price_per_1m"` // USD per 1M output tokens
	ContextWindow    int     `json:"context_window"`      // max tokens (input + output)
}
