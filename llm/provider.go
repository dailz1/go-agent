package llm

import (
	"context"
	"errors"
	"iter"

	"github.com/dailz1/go-agent/tool"
)

// ErrStreamingNotSupported is returned by providers that do not implement
// streaming. Callers can check this with errors.Is to provide a fallback
// to the non-streaming Chat method.
var ErrStreamingNotSupported = errors.New("streaming not supported by this provider")

// Provider is the interface that every LLM provider (OpenAI, Anthropic, etc.)
// must implement. It translates provider-agnostic Messages into provider-specific
// wire formats and back.
type Provider interface {
	Name() string
	Chat(ctx context.Context, messages []Message, tools []tool.ToolInfo, opts ...Option) (*Message, *Usage, error)
	// ChatStream sends a streaming chat completion request and returns an
	// iterator that lazily yields [Chunk] values as they arrive from the
	// provider. Providers that do not support streaming should return
	// (nil, ErrStreamingNotSupported).
	//
	// The returned iter.Seq2[Chunk, error] is lazy — no network activity
	// occurs until the caller begins ranging. Breaking out of the range
	// early is safe.
	ChatStream(ctx context.Context, messages []Message, tools []tool.ToolInfo, opts ...Option) (iter.Seq2[Chunk, error], error)
}

type Option func(*Options)

type Options struct {
	Model       string
	MaxTokens   int
	Temperature *float64
	Stop        []string
	// Thinking is passed through as the "type" of the request's thinking
	// configuration (e.g. "enabled", "disabled"). Adapters that do not
	// support a thinking wire field ignore it.
	Thinking string
	// JSONMode requests JSON-object output via response_format on adapters
	// that support it.
	JSONMode bool
}

func WithModel(model string) Option {
	return func(o *Options) { o.Model = model }
}

func WithMaxTokens(n int) Option {
	return func(o *Options) { o.MaxTokens = n }
}

func WithTemperature(t float64) Option {
	return func(o *Options) { o.Temperature = &t }
}

func WithStop(stop ...string) Option {
	return func(o *Options) { o.Stop = stop }
}

// WithThinking sets the thinking mode for the call (e.g. "enabled",
// "disabled"). It overrides a constructor-level thinking setting on
// adapters that support both.
func WithThinking(t string) Option {
	return func(o *Options) { o.Thinking = t }
}

// WithJSONMode requests JSON-object output from the model.
func WithJSONMode() Option {
	return func(o *Options) { o.JSONMode = true }
}

func ApplyOptions(opts []Option) Options {
	var o Options
	for _, opt := range opts {
		opt(&o)
	}
	return o
}
