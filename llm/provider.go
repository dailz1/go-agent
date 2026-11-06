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
	// iterator that yields [Chunk] values as they arrive from the provider.
	// Providers that do not support streaming should return
	// (nil, ErrStreamingNotSupported).
	//
	// Request setup (connection, auth, non-2xx status) may be performed
	// eagerly at call time or lazily when ranging begins, per adapter.
	// Setup errors SHOULD be returned synchronously so caller-side retry
	// loops cover them; all four adapters in this package (glm, openai,
	// openairesponses, codex) are eager — codex resolves OAuth credentials
	// and performs its one-shot 401 recovery during this synchronous setup.
	//
	// Chunk delivery through the returned iter.Seq2[Chunk, error] is lazy:
	// chunks are consumed only while the caller ranges, and breaking out
	// of the range early is safe. Eager adapters hold the response body
	// open from the successful return until the caller ranges (to
	// completion or an early break) or cancels the context; deferring the
	// first range holds the connection and consumes the client timeout
	// budget.
	ChatStream(ctx context.Context, messages []Message, tools []tool.ToolInfo, opts ...Option) (iter.Seq2[Chunk, error], error)
}

type Option func(*Options)

// ToolChoiceMode selects how the provider constrains tool calling for a
// request: none, auto, or required.
type ToolChoiceMode string

const (
	// ToolChoiceNone forbids the model from calling tools.
	ToolChoiceNone ToolChoiceMode = "none"
	// ToolChoiceAuto lets the model decide (provider default with tools).
	ToolChoiceAuto ToolChoiceMode = "auto"
	// ToolChoiceRequired forces the model to produce at least one tool
	// call. Footgun: an agent loop driven entirely by required choices
	// never sees a tool-free terminal reply, so it can only terminate via
	// its iteration limit.
	ToolChoiceRequired ToolChoiceMode = "required"
)

// ToolChoice constrains the model's tool calling. Name non-empty selects a
// specific tool (serialized as the provider's named-function form); Name
// empty selects by Mode. The zero value means "no constraint" and is never
// serialized. Use the WithToolChoice* option constructors instead of struct
// literals: they cannot express invalid combinations.
type ToolChoice struct {
	Mode ToolChoiceMode
	Name string
}

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

	// MaxCompletionTokens sets the provider's max_completion_tokens wire
	// field (OpenAI o-series/gpt-5 family rejects legacy max_tokens). When
	// set, adapters suppress MaxTokens entirely: the two are mutually
	// exclusive on the wire. Adapters whose wire format lacks the field
	// (GLM) ignore it.
	MaxCompletionTokens *int64
	// TopP sets the top_p sampling parameter. Adapters that already carry
	// a constructor-level top_p keep it as the default and let this
	// call-level value override it.
	TopP *float64
	// Seed requests best-effort deterministic sampling: repeated requests
	// with the same seed and parameters tend to return the same result,
	// but determinism is NOT guaranteed and depends on the provider
	// (OpenAI marks the field beta/deprecated on its newest models;
	// DeepSeek/Ollama/vLLM-compatible endpoints honor it).
	Seed *int64
	// PresencePenalty sets the presence_penalty sampling parameter.
	PresencePenalty *float64
	// FrequencyPenalty sets the frequency_penalty sampling parameter.
	FrequencyPenalty *float64
	// ParallelToolCalls constrains whether the model may request multiple
	// tool calls in a single reply. It is independent of the agent layer's
	// own parallel tool execution.
	ParallelToolCalls *bool
	// ToolChoice constrains the model's tool calling (see ToolChoice).
	// Construct it only via WithToolChoiceNone/Auto/Required/Tool.
	ToolChoice *ToolChoice
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

// WithMaxCompletionTokens sets max_completion_tokens (OpenAI o-series/gpt-5
// family). Setting it suppresses MaxTokens on adapters that send both.
func WithMaxCompletionTokens(n int64) Option {
	return func(o *Options) { o.MaxCompletionTokens = &n }
}

// WithTopP sets the top_p sampling parameter for the call.
func WithTopP(p float64) Option {
	return func(o *Options) { o.TopP = &p }
}

// WithSeed requests best-effort deterministic sampling (see Options.Seed).
func WithSeed(n int64) Option {
	return func(o *Options) { o.Seed = &n }
}

// WithPresencePenalty sets the presence_penalty sampling parameter.
func WithPresencePenalty(p float64) Option {
	return func(o *Options) { o.PresencePenalty = &p }
}

// WithFrequencyPenalty sets the frequency_penalty sampling parameter.
func WithFrequencyPenalty(p float64) Option {
	return func(o *Options) { o.FrequencyPenalty = &p }
}

// WithParallelToolCalls constrains whether the model may request multiple
// tool calls in a single reply.
func WithParallelToolCalls(b bool) Option {
	return func(o *Options) { o.ParallelToolCalls = &b }
}

// WithToolChoiceNone forbids the model from calling tools this request.
func WithToolChoiceNone() Option {
	return func(o *Options) { o.ToolChoice = &ToolChoice{Mode: ToolChoiceNone} }
}

// WithToolChoiceAuto lets the model decide whether to call tools.
func WithToolChoiceAuto() Option {
	return func(o *Options) { o.ToolChoice = &ToolChoice{Mode: ToolChoiceAuto} }
}

// WithToolChoiceRequired forces the model to produce at least one tool
// call. Footgun: an agent loop driven entirely by required choices never
// sees a tool-free terminal reply and can only end via its iteration
// limit.
func WithToolChoiceRequired() Option {
	return func(o *Options) { o.ToolChoice = &ToolChoice{Mode: ToolChoiceRequired} }
}

// WithToolChoiceTool forces the model to call the named tool. An empty name
// sets nothing (an empty name cannot address a tool).
func WithToolChoiceTool(name string) Option {
	if name == "" {
		return func(o *Options) {}
	}
	return func(o *Options) { o.ToolChoice = &ToolChoice{Name: name} }
}

func ApplyOptions(opts []Option) Options {
	var o Options
	for _, opt := range opts {
		opt(&o)
	}
	return o
}
