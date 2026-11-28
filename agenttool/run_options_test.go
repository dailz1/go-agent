package agenttool_test

import (
	"context"
	"fmt"
	"iter"
	"sync"
	"testing"

	"github.com/dailz1/go-agent/agent"
	"github.com/dailz1/go-agent/agenttool"
	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

type budgetProvider struct {
	mu      sync.Mutex
	counts  map[string]int
	started chan struct{}
	release chan struct{}
}

func (*budgetProvider) Name() string { return "budget" }
func (*budgetProvider) Chat(context.Context, []llm.Message, []tool.ToolInfo, ...llm.Option) (*llm.Message, *llm.Usage, error) {
	return nil, nil, llm.ErrStreamingNotSupported
}
func (p *budgetProvider) ChatStream(ctx context.Context, messages []llm.Message, _ []tool.ToolInfo, _ ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	key := messages[0].Content[0].(*llm.TextBlock).Text
	p.mu.Lock()
	n := p.counts[key]
	p.counts[key]++
	p.mu.Unlock()
	if n == 0 {
		select {
		case p.started <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		select {
		case <-p.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return func(yield func(llm.Chunk, error) bool) {
		for _, chunk := range []llm.Chunk{
			&llm.ToolCallStartChunk{Index: 0, ID: fmt.Sprintf("%s-%d", key, n), Name: "echo"},
			&llm.ToolCallArgsChunk{Index: 0, Delta: "{}"}, &llm.DoneChunk{FinishReason: "tool_calls"},
		} {
			if !yield(chunk, nil) {
				return
			}
		}
	}, nil
}

func TestRunOptionsBudgetPassthrough(t *testing.T) {
	p := &budgetProvider{counts: make(map[string]int), started: make(chan struct{}, 2), release: make(chan struct{})}
	child := agent.New(p, tool.NewRegistry(), agent.WithMaxIter(10))
	type outcome struct {
		result *tool.ToolResult
		err    error
	}
	results := make(chan outcome, 2)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	for _, tc := range []struct {
		name   string
		budget int
	}{{name: "one", budget: 1}, {name: "three", budget: 3}} {
		adapter := agenttool.New(child, agenttool.Config{Name: tc.name, RunOptions: []agent.RunOption{agent.RunWithMaxIter(tc.budget)}})
		go func() {
			result, err := adapter.Execute(ctx, []byte(fmt.Sprintf(`{"input":%q}`, tc.name)))
			results <- outcome{result: result, err: err}
		}()
	}
	await(t, p.started)
	await(t, p.started)
	close(p.release)
	for range 2 {
		got := await(t, results)
		if got.err != nil || got.result == nil || got.result.Status != tool.ResultError {
			t.Fatalf("truncation must remain a soft result: %+v", got)
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.counts["one"] != 1 || p.counts["three"] != 3 || len(p.counts) != 2 {
		t.Fatalf("child requests=%v, want one:1 three:3", p.counts)
	}
}
