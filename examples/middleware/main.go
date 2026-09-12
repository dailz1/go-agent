// Command middleware demonstrates the P2-1 wrapper conventions from
// docs/DESIGN.md §4: observe via events, change behavior by wrapping
// Tool / Provider. No kernel types or hook chains are introduced.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"log/slog"

	"github.com/dailz1/go-agent/pkg/llm"
	"github.com/dailz1/go-agent/pkg/tool"
)

// loggingTool wraps a tool.Tool for observation only: full ToolInfo
// passthrough, original ctx/args, metadata-only logging, %w error wrapping.
type loggingTool struct {
	tool.Tool
	log *slog.Logger
}

func (t loggingTool) Execute(ctx context.Context, args json.RawMessage) (*tool.ToolResult, error) {
	t.log.Info("tool.execute", "name", t.Tool.Info().Name, "args_bytes", len(args))
	res, err := t.Tool.Execute(ctx, args) // ctx and args passed through unchanged
	if err != nil {
		return nil, fmt.Errorf("loggingTool %s: %w", t.Tool.Info().Name, err)
	}
	t.log.Info("tool.result", "name", t.Tool.Info().Name, "content_bytes", len(res.Content))
	return res, nil
}

// loggingProvider wraps an llm.Provider for observation only: messages,
// tools, options, usage and errors (outer and in-iteration) pass through.
type loggingProvider struct {
	llm.Provider
	log *slog.Logger
}

func (p loggingProvider) Chat(ctx context.Context, messages []llm.Message, tools []tool.ToolInfo, opts ...llm.Option) (*llm.Message, *llm.Usage, error) {
	p.log.Info("provider.chat", "provider", p.Provider.Name(), "messages", len(messages), "tools", len(tools))
	msg, usage, err := p.Provider.Chat(ctx, messages, tools, opts...)
	if err != nil {
		return nil, nil, fmt.Errorf("loggingProvider %s: %w", p.Provider.Name(), err)
	}
	p.log.Info("provider.chat.done", "usage", usage != nil)
	return msg, usage, nil
}

func (p loggingProvider) ChatStream(ctx context.Context, messages []llm.Message, tools []tool.ToolInfo, opts ...llm.Option) (seq iter.Seq2[llm.Chunk, error], err error) {
	p.log.Info("provider.chat_stream", "provider", p.Provider.Name(), "messages", len(messages), "tools", len(tools))
	inner, err := p.Provider.ChatStream(ctx, messages, tools, opts...)
	if err != nil {
		// Never swallow sentinels such as llm.ErrStreamingNotSupported:
		// the agent relies on errors.Is for non-stream fallback.
		return nil, fmt.Errorf("loggingProvider %s: %w", p.Provider.Name(), err)
	}
	// Return a lazy iterator: no ranging here, early break is safe.
	return func(yield func(llm.Chunk, error) bool) {
		for chunk, iterErr := range inner {
			if iterErr != nil {
				iterErr = fmt.Errorf("loggingProvider %s: %w", p.Provider.Name(), iterErr)
			}
			if !yield(chunk, iterErr) {
				return
			}
		}
	}, nil
}

// rateLimitStreamProvider holds a rate-limit permit for the entire lifetime
// of the stream iterator — released on natural exhaustion AND on early break.
type rateLimitStreamProvider struct {
	llm.Provider
	sem chan struct{}
}

func NewRateLimitStreamProvider(p llm.Provider, maxStreams int) rateLimitStreamProvider {
	return rateLimitStreamProvider{Provider: p, sem: make(chan struct{}, maxStreams)}
}

func (p rateLimitStreamProvider) ChatStream(ctx context.Context, messages []llm.Message, tools []tool.ToolInfo, opts ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	inner, err := p.Provider.ChatStream(ctx, messages, tools, opts...)
	if err != nil {
		return nil, err // passthrough, %w via loggingProvider if wrapped there
	}
	select {
	case p.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	// Lazy: permit already held; released only when iteration ends
	// (naturally or via early break).
	return func(yield func(llm.Chunk, error) bool) {
		defer func() { <-p.sem }()
		for chunk, iterErr := range inner {
			if !yield(chunk, iterErr) {
				return // early break: defer releases the permit
			}
		}
	}, nil
}

func main() {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil)) // discardable default

	provider := loggingProvider{Provider: NewRateLimitStreamProvider(&demoProvider{}, 2), log: logger}
	_ = provider
	fmt.Println("see main_test.go for behavior; run with a real provider to use.")
}

// demoProvider is a minimal in-memory provider so the example stays
// self-contained and dependency-free.
type demoProvider struct{}

func (d *demoProvider) Name() string { return "demo" }

func (d *demoProvider) Chat(ctx context.Context, messages []llm.Message, tools []tool.ToolInfo, opts ...llm.Option) (*llm.Message, *llm.Usage, error) {
	return &llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock{Type: "text", Text: "demo"}}}, nil, nil
}

func (d *demoProvider) ChatStream(ctx context.Context, messages []llm.Message, tools []tool.ToolInfo, opts ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	return func(yield func(llm.Chunk, error) bool) {
		if !yield(llm.TextDeltaChunk{Text: "hello"}, nil) {
			return
		}
		yield(llm.DoneChunk{}, nil)
	}, nil
}
