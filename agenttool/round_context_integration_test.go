package agenttool_test

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/agent"
	"github.com/dailz1/go-agent/agenttool"
	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

type roundContextSequenceProvider struct {
	steps    [][]llm.Chunk
	requests [][]llm.Message
}

func (*roundContextSequenceProvider) Name() string { return "round-context-sequence" }

func (p *roundContextSequenceProvider) Chat(context.Context, []llm.Message, []tool.ToolInfo, ...llm.Option) (*llm.Message, *llm.Usage, error) {
	return nil, nil, llm.ErrStreamingNotSupported
}

func (p *roundContextSequenceProvider) ChatStream(_ context.Context, messages []llm.Message, _ []tool.ToolInfo, _ ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	p.requests = append(p.requests, append([]llm.Message(nil), messages...))
	if len(p.steps) == 0 {
		return nil, errors.New("unexpected ChatStream call")
	}
	chunks := p.steps[0]
	p.steps = p.steps[1:]
	return func(yield func(llm.Chunk, error) bool) {
		for _, chunk := range chunks {
			if !yield(chunk, nil) {
				return
			}
		}
	}, nil
}

func TestAgentToolRoundContextProviderDoesNotInherit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		parentSnapshot   string
		childSnapshot    string
		wantChildOverlay string
	}{
		{name: "parent does not configure child", parentSnapshot: "parent-snapshot"},
		{name: "child uses only its own option", childSnapshot: "child-snapshot", wantChildOverlay: "child-snapshot"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			childProvider := &roundContextSequenceProvider{steps: [][]llm.Chunk{{llm.TextDeltaChunk{Text: "child"}, llm.DoneChunk{FinishReason: "stop"}}}}
			childOptions := []agent.Option{}
			if tt.childSnapshot != "" {
				childOptions = append(childOptions, agent.WithRoundContextProvider(func(context.Context, agent.RoundContextRequest) (string, error) {
					return tt.childSnapshot, nil
				}))
			}
			child := agent.New(childProvider, tool.NewRegistry(), childOptions...)
			registry := tool.NewRegistry()
			registry.MustRegister(agenttool.New(child, agenttool.Config{Name: "child"}))
			parentProvider := &roundContextSequenceProvider{steps: [][]llm.Chunk{
				{llm.ToolCallStartChunk{ID: "call", Name: "child"}, llm.ToolCallArgsChunk{ID: "call", Delta: `{"input":"work"}`}, llm.DoneChunk{FinishReason: "tool_calls"}},
				{llm.TextDeltaChunk{Text: "parent"}, llm.DoneChunk{FinishReason: "stop"}},
			}}
			parentOptions := []agent.Option{}
			if tt.parentSnapshot != "" {
				parentOptions = append(parentOptions, agent.WithRoundContextProvider(func(context.Context, agent.RoundContextRequest) (string, error) {
					return tt.parentSnapshot, nil
				}))
			}
			if _, err := agent.New(parentProvider, registry, parentOptions...).Run(context.Background(), "parent work"); err != nil {
				t.Fatalf("parent Run() error = %v", err)
			}
			got := textOf(childProvider.requests[0][len(childProvider.requests[0])-1])
			if tt.wantChildOverlay == "" && got != "work" {
				t.Errorf("child outbound text = %q, want no inherited overlay", got)
			}
			if tt.wantChildOverlay != "" && !strings.Contains(got, tt.wantChildOverlay) {
				t.Errorf("child outbound text = %q, want its own overlay %q", got, tt.wantChildOverlay)
			}
		})
	}
}

func TestAgentToolNestedRunInterruptedErrorKeepsParentThread(t *testing.T) {
	childErr := errors.New("child snapshot failed")
	child := agent.New(nil, tool.NewRegistry(),
		agent.WithStore(store.NewMemory()),
		agent.WithRoundContextProvider(func(context.Context, agent.RoundContextRequest) (string, error) {
			return "", childErr
		}),
	)
	registry := tool.NewRegistry()
	registry.MustRegister(agenttool.New(child, agenttool.Config{Name: "child"}))
	parent := agent.New(&roundContextSequenceProvider{steps: [][]llm.Chunk{{
		llm.ToolCallStartChunk{ID: "call", Name: "child"},
		llm.ToolCallArgsChunk{ID: "call", Delta: string(json.RawMessage(`{"input":"work"}`))},
		llm.DoneChunk{FinishReason: "tool_calls"},
	}}}, registry, agent.WithStore(store.NewMemory()))

	_, err := parent.RunThread(context.Background(), "parent-thread", "parent work")
	var threadIDs []string
	for current := err; current != nil; current = errors.Unwrap(current) {
		if interrupted, ok := current.(*agent.RunInterruptedError); ok {
			threadIDs = append(threadIDs, interrupted.ThreadID)
		}
	}
	if !errors.Is(err, childErr) || len(threadIDs) != 2 || threadIDs[0] != "parent-thread" || threadIDs[1] == "parent-thread" {
		t.Fatalf("error chain = %v, thread IDs = %v; want parent then distinct child wrapper", err, threadIDs)
	}
}

func textOf(message llm.Message) string {
	text := ""
	for _, block := range message.Content {
		if block, ok := block.(llm.TextBlock); ok {
			text += block.Text
		}
	}
	return text
}
