// Package openairesponses implements the llm.Provider interface against
// OpenAI's Responses protocol (POST /v1/responses): typed input/output items
// instead of messages, entry-level reasoning items, and semantic streaming
// events.
//
// Statefulness policy (v1): requests are sent with store:false and the full
// item history is replayed on every call — history stays owned by the
// caller, orthogonal to compaction, truncation, and the durable store.
// Reasoning items are returned with encrypted_content so stateless replay
// keeps reasoning continuity across tool rounds.
//
// Not supported in v1: OpenAI built-in tools (web search, file search, code
// interpreter, computer use, MCP), structured outputs (text.format),
// previous_response_id, and WebSocket mode.
package openairesponses

import (
	"log/slog"
	"net/http"
	"time"
)

const defaultBaseURL = "https://api.openai.com/v1"

// Provider implements llm.Provider against the Responses protocol.
type Provider struct {
	apiKey     string
	model      string
	baseURL    string
	httpClient *http.Client
	logger     *slog.Logger
}

// ProviderOption configures the Responses provider.
type ProviderOption func(*Provider)

// WithBaseURL overrides the API base URL (default https://api.openai.com/v1).
// Any OpenAI-compatible Responses endpoint works.
func WithBaseURL(url string) ProviderOption {
	return func(p *Provider) { p.baseURL = url }
}

// WithHTTPClient overrides the HTTP client.
func WithHTTPClient(c *http.Client) ProviderOption {
	return func(p *Provider) { p.httpClient = c }
}

// WithLogger sets the structured logger.
func WithLogger(l *slog.Logger) ProviderOption {
	return func(p *Provider) { p.logger = l }
}

// NewProvider creates a Responses-protocol provider. The default HTTP client
// carries a 120s timeout: an unbounded client could block a run forever on a
// wedged connection.
func NewProvider(apiKey, model string, opts ...ProviderOption) *Provider {
	p := &Provider{
		apiKey:     apiKey,
		model:      model,
		baseURL:    defaultBaseURL,
		httpClient: &http.Client{Timeout: 120 * time.Second},
		logger:     slog.Default(),
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Name implements llm.Provider.
func (p *Provider) Name() string { return "openai_responses" }

func (p *Provider) apiHeaders() map[string]string {
	return map[string]string{
		"Authorization": "Bearer " + p.apiKey,
	}
}
