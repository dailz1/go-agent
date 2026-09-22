// Package codex implements ChatGPT subscription Responses requests.
// Authentication and network activity start only when the stream is consumed.
// Chat folds that same stream. Only a pre-stream 401 is recovered, once; other
// HTTP and stream failures are not retried (the agent's outer retry loop does
// not retry lazy iterator errors). All system text blocks become instructions,
// separated by blank lines, unlike openairesponses' last-system behavior.
//
// MaxTokens is accepted but never transmitted: the service controls the output
// limit. Temperature and Stop are unsupported. This provider never falls back
// to API-key billing, discovers credentials, or changes the caller's account.
package codex

import (
	"context"
	"encoding/json"
	"iter"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/llm/codex/auth"
	"github.com/dailz1/go-agent/tool"
)

const defaultBaseURL = "https://chatgpt.com/backend-api/codex"

// Provider implements llm.Provider using an explicitly supplied auth source.
type Provider struct {
	model      string
	baseURL    string
	originator string
	source     auth.Source
	httpClient *http.Client
	logger     *slog.Logger
	warnLimit  sync.Once
}

// ProviderOption configures a subscription provider.
type ProviderOption func(*Provider)

// WithAuthSource supplies the sole owner of credential refresh and persistence.
func WithAuthSource(source auth.Source) ProviderOption {
	return func(p *Provider) { p.source = source }
}

// WithBaseURL overrides the destination to which credentials will be sent.
func WithBaseURL(baseURL string) ProviderOption { return func(p *Provider) { p.baseURL = baseURL } }

// WithHTTPClient uses a copy of client with redirects disabled.
func WithHTTPClient(client *http.Client) ProviderOption {
	return func(p *Provider) { p.httpClient = client }
}

// WithLogger selects the logger for the once-per-provider output-limit warning.
func WithLogger(logger *slog.Logger) ProviderOption { return func(p *Provider) { p.logger = logger } }

// WithOriginator explicitly selects a client identifier; the default is go_agent.
func WithOriginator(originator string) ProviderOption {
	return func(p *Provider) { p.originator = originator }
}

// NewProvider constructs a provider without I/O. Configuration errors are
// reported during local Chat/ChatStream preparation.
func NewProvider(model string, options ...ProviderOption) *Provider {
	p := &Provider{model: model, baseURL: defaultBaseURL, originator: "go_agent",
		httpClient: &http.Client{Timeout: 120 * time.Second}, logger: slog.Default()}
	for _, option := range options {
		option(p)
	}
	if p.httpClient != nil {
		client := *p.httpClient
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		p.httpClient = &client
	}
	if p.logger == nil {
		p.logger = slog.Default()
	}
	return p
}

func (p *Provider) Name() string { return "codex" }

// ChatStream prepares locally, then authenticates and sends only when ranged.
func (p *Provider) ChatStream(ctx context.Context, messages []llm.Message, tools []tool.ToolInfo, options ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	o := llm.ApplyOptions(options)
	model := p.model
	if o.Model != "" {
		model = o.Model
	}
	endpoint, err := p.endpoint()
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(model) == "" {
		return nil, capabilityError("model_required")
	}
	if o.MaxTokens < 0 {
		return nil, capabilityError("negative_max_tokens")
	}
	if o.Temperature != nil {
		return nil, capabilityError("temperature_unsupported")
	}
	if len(o.Stop) > 0 {
		return nil, capabilityError("stop_unsupported")
	}
	items, instructions, err := convertMessages(messages)
	if err != nil {
		return nil, err
	}
	defs, err := convertTools(tools)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(createRequest{Model: model, Instructions: instructions, Input: items, Tools: defs,
		Stream: true, Store: false, Include: []string{"reasoning.encrypted_content"}})
	if err != nil {
		return nil, err
	}
	return func(yield func(llm.Chunk, error) bool) {
		if o.MaxTokens > 0 {
			p.warnLimit.Do(func() {
				p.logger.WarnContext(ctx, "Codex uses the server output limit",
					"configured_max_output_tokens", o.MaxTokens, "output_limit_mode", "server_default")
			})
		}
		result, err := p.openStream(ctx, endpoint, body)
		if err != nil {
			yield(nil, err)
			return
		}
		defer result.Cleanup()
		for chunk, err := range scanResponse(ctx, result.Body) {
			if !yield(chunk, err) {
				return
			}
		}
	}, nil
}

func (p *Provider) endpoint() (string, error) {
	if p.source == nil || (reflect.ValueOf(p.source).Kind() == reflect.Pointer && reflect.ValueOf(p.source).IsNil()) {
		return "", capabilityError("auth_source_required")
	}
	if p.httpClient == nil || p.originator == "" || strings.ContainsAny(p.originator, "\r\n") {
		return "", capabilityError("invalid_client")
	}
	u, err := url.Parse(p.baseURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return "", capabilityError("invalid_base_url")
	}
	if u.Scheme != "https" {
		ip := net.ParseIP(u.Hostname())
		if u.Scheme != "http" || (u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback())) {
			return "", capabilityError("insecure_base_url")
		}
	}
	u.Path = strings.TrimRight(u.Path, "/")
	if !strings.HasSuffix(u.Path, "/responses") {
		u.Path += "/responses"
	}
	u.RawPath = ""
	return u.String(), nil
}

var _ llm.Provider = (*Provider)(nil)
