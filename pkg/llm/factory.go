package llm

import (
	"fmt"
	"log/slog"
	"sort"
)

// ProviderConfig holds configuration for creating a provider instance.
type ProviderConfig struct {
	APIKey  string
	Model   string
	BaseURL string
	Logger  *slog.Logger
}

// ProviderFactory creates a Provider from config.
type ProviderFactory func(ProviderConfig) (Provider, error)

// providerRegistry maps provider names to factory functions.
// NOT thread-safe. Registration must happen during startup only,
// never concurrently with CreateProvider calls.
var providerRegistry = map[string]ProviderFactory{}

// RegisterProvider registers a factory for a provider name.
// Panics on duplicate registration.
func RegisterProvider(name string, factory ProviderFactory) {
	if _, exists := providerRegistry[name]; exists {
		panic(fmt.Sprintf("provider %q already registered", name))
	}
	providerRegistry[name] = factory
}

// CreateProvider creates a provider instance by name.
func CreateProvider(name string, config ProviderConfig) (Provider, error) {
	factory, ok := providerRegistry[name]
	if !ok {
		return nil, fmt.Errorf("unknown provider %q (available: %v)", name, RegisteredProviders())
	}
	return factory(config)
}

// RegisteredProviders returns a sorted list of registered provider names.
func RegisteredProviders() []string {
	names := make([]string, 0, len(providerRegistry))
	for name := range providerRegistry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// resetRegistry clears all registered providers.
// For testing only.
func resetRegistry() {
	providerRegistry = map[string]ProviderFactory{}
}
