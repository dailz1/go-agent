package openairesponses

// WithModelContextWindows registers per-model context windows (tokens) for
// this provider. The map is copied; non-positive entries are dropped and
// treated as undeclared. A later call replaces the whole registration.
func WithModelContextWindows(windows map[string]int) ProviderOption {
	filtered := make(map[string]int, len(windows))
	for model, tokens := range windows {
		if tokens > 0 {
			filtered[model] = tokens
		}
	}
	return func(p *Provider) { p.contextWindows = filtered }
}

// ContextWindow implements llm.ContextWindowProvider. An empty model
// resolves to the constructor default model; matching is exact.
func (p *Provider) ContextWindow(model string) (int, bool) {
	if model == "" {
		model = p.model
	}
	tokens, ok := p.contextWindows[model]
	return tokens, ok
}
