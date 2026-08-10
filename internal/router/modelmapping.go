package router

// ModelMapping resolves an equivalent model on a target provider type (§4.1).
// The structure mirrors the config block: model → provider TYPE → mapped model
// (e.g. "gpt-4o" → "anthropic" → "claude-sonnet-4-20250514").
type ModelMapping map[string]map[string]string

// MappedModel returns the equivalent of model on providerType, or the model
// unchanged when no mapping exists (a provider instance serving the model
// natively is still a valid candidate).
func (m ModelMapping) MappedModel(model, providerType string) string {
	if targets, ok := m[model]; ok {
		if mapped, ok := targets[providerType]; ok && mapped != "" {
			return mapped
		}
	}
	return model
}
