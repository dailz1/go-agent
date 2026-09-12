package agenttest

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"sync"
	"testing"
	"time"

	"github.com/dailz1/go-agent/pkg/agent"
	"github.com/dailz1/go-agent/pkg/llm"
	"github.com/dailz1/go-agent/pkg/tool"
)

func TestToolFuncRecordsDeepCopiedConcurrentCalls(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	toolFunc := &ToolFunc{Definition: tool.ToolInfo{Name: "test"}, ExecuteFunc: func(_ context.Context, args json.RawMessage) (*tool.ToolResult, error) {
		started <- struct{}{}
		<-release
		return tool.NewTextResult(string(args)), nil
	}}
	var group sync.WaitGroup
	for _, args := range []json.RawMessage{[]byte(`{"n":1}`), []byte(`{"n":2}`)} {
		group.Go(func() {
			_, err := toolFunc.Execute(context.Background(), args)
			if err != nil {
				t.Errorf("Execute: %v", err)
			}
		})
	}
	waitToolSignal(t, started)
	waitToolSignal(t, started)
	close(release)
	group.Wait()
	calls := toolFunc.Calls()
	if len(calls) != 2 {
		t.Fatalf("calls = %#v", calls)
	}
	calls[0].Args[0] = 'x'
	if toolFunc.Calls()[0].Args[0] == 'x' {
		t.Fatal("Calls returned aliased args")
	}
}

func TestToolFuncNilHandlerAndPanic(t *testing.T) {
	if _, err := (&ToolFunc{}).Execute(context.Background(), nil); err == nil {
		t.Fatal("nil handler succeeded")
	}
	toolFunc := &ToolFunc{ExecuteFunc: func(context.Context, json.RawMessage) (*tool.ToolResult, error) { panic("expected") }}
	defer func() {
		if recovered := recover(); recovered != "expected" {
			t.Fatalf("panic = %#v", recovered)
		}
	}()
	_, _ = toolFunc.Execute(context.Background(), nil)
}

func TestToolFuncWorksWithParallelAgentExecution(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	registry := tool.NewRegistry()
	for _, name := range []string{"one", "two"} {
		name := name
		if err := registry.Register(&ToolFunc{Definition: tool.ToolInfo{Name: name}, ExecuteFunc: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
			started <- struct{}{}
			<-release
			return tool.NewTextResult(name), nil
		}}); err != nil {
			t.Fatal(err)
		}
	}
	provider := &parallelToolProvider{}
	result := make(chan error, 1)
	go func() {
		_, err := agent.New(provider, registry, agent.WithToolConcurrency(2)).Run(context.Background(), "run")
		result <- err
	}()
	waitToolSignal(t, started)
	waitToolSignal(t, started)
	close(release)
	if err := waitToolResult(t, result); err != nil {
		t.Fatal(err)
	}
	if got := len(provider.Calls()); got != 2 {
		t.Fatalf("tool calls = %d", got)
	}
}

func TestToolFuncReturnsHandlerErrors(t *testing.T) {
	expected := errors.New("hard error")
	toolFunc := &ToolFunc{ExecuteFunc: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewErrorResult("soft"), expected
	}}
	result, err := toolFunc.Execute(context.Background(), json.RawMessage(`{}`))
	if !errors.Is(err, expected) || result == nil || !result.IsError() {
		t.Fatalf("result=%#v error=%v", result, err)
	}
}

func waitToolSignal(t *testing.T, signal <-chan struct{}) {
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for tool signal")
	}
}
func waitToolResult(t *testing.T, result <-chan error) error {
	select {
	case err := <-result:
		return err
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for agent result")
		return nil
	}
}

type parallelToolProvider struct {
	mu    sync.Mutex
	calls int
}

func (*parallelToolProvider) Name() string { return "parallel-tool" }
func (*parallelToolProvider) Chat(context.Context, []llm.Message, []tool.ToolInfo, ...llm.Option) (*llm.Message, *llm.Usage, error) {
	return nil, nil, llm.ErrStreamingNotSupported
}
func (p *parallelToolProvider) ChatStream(context.Context, []llm.Message, []tool.ToolInfo, ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	p.mu.Lock()
	p.calls++
	call := p.calls
	p.mu.Unlock()
	if call == 1 {
		return func(yield func(llm.Chunk, error) bool) {
			for _, chunk := range []llm.Chunk{llm.ToolCallStartChunk{Index: 0, ID: "one", Name: "one"}, llm.ToolCallStartChunk{Index: 1, ID: "two", Name: "two"}, llm.ToolCallArgsChunk{Index: 0, ID: "one", Delta: "{}"}, llm.ToolCallArgsChunk{Index: 1, ID: "two", Delta: "{}"}} {
				if !yield(chunk, nil) {
					return
				}
			}
		}, nil
	}
	return func(yield func(llm.Chunk, error) bool) { yield(llm.TextDeltaChunk{Text: "done"}, nil) }, nil
}
func (p *parallelToolProvider) Calls() []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return make([]int, p.calls)
}
