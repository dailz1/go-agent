package llm

import "context"

// Embedder is the embedding counterpart of [Provider]: a provider-side
// endpoint that turns texts into vectors. It is a separate small interface,
// NOT a method on Provider — chat-only adapters are unaffected and providers
// without an embedding endpoint simply do not implement it. The agent loop
// never consumes embeddings; this surface serves applications that reuse the
// same provider management (auth, base URL, error classification) as their
// chat calls.
type Embedder interface {
	// Name identifies the adapter, mirroring [Provider.Name].
	Name() string
	// Embed vectorizes input texts. The returned slice is aligned with
	// input positionally: result[i] vectorizes input[i]. One Embed call
	// maps to exactly one HTTP request — callers batch themselves and
	// respect provider batch limits (e.g. OpenAI caps a single request
	// at 2048 inputs). An empty input returns an error without any
	// HTTP call. Errors are *APIError (leaf, Retryable() classifies
	// 429/5xx) for non-2xx responses, or wrapped network errors.
	Embed(ctx context.Context, input []string, opts ...EmbedOption) ([][]float32, Usage, error)
}

// EmbedOptions carries the per-call parameters for [Embedder.Embed].
// It is deliberately independent of [Options]: most chat parameters
// (temperature, stop sequences, thinking) have no embedding counterpart.
type EmbedOptions struct {
	// Model is the embedding model to invoke. Constructors take a
	// default; WithEmbedModel overrides it per call.
	Model string
	// Dimensions requests a shortened vector when the provider supports
	// it (e.g. OpenAI text-embedding-3-*). Zero omits the wire field.
	Dimensions int
}

// EmbedOption configures a single Embed call.
type EmbedOption func(*EmbedOptions)

// WithEmbedModel overrides the embedding model for this call.
func WithEmbedModel(model string) EmbedOption {
	return func(o *EmbedOptions) { o.Model = model }
}

// WithDimensions requests vectors of the given dimensionality for this
// call. Only meaningful on providers that support shortened embeddings;
// adapters that do not support the wire field ignore it.
func WithDimensions(n int) EmbedOption {
	return func(o *EmbedOptions) { o.Dimensions = n }
}
