package agenttest

import (
	"context"
	"encoding/json"
	"iter"
	"sync"
	"testing"

	"github.com/dailz1/go-agent/pkg/agent"
	"github.com/dailz1/go-agent/pkg/llm"
	"github.com/dailz1/go-agent/pkg/tool"
)

func TestToolFuncWorksWithSerialAgentExecution(t *testing.T) {
	registry := tool.NewRegistry()
	toolFunc := &ToolFunc{Definition: tool.ToolInfo{Name: "one"}, ExecuteFunc: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult("one"), nil
	}}
	registry.MustRegister(toolFunc)
	result, err := agent.New(&singleToolProvider{}, registry).Run(t.Context(), "run")
	if err != nil || result.Message.Content[0].(llm.TextBlock).Text != "done" {
		t.Fatalf("Run = %#v, %v", result, err)
	}
	if calls := toolFunc.Calls(); len(calls) != 1 || string(calls[0].Args) != "{}" {
		t.Fatalf("calls = %#v", calls)
	}
}

func TestAgentRecoversToolFuncPanic(t *testing.T) {
	registry := tool.NewRegistry()
	toolFunc := &ToolFunc{Definition: tool.ToolInfo{Name: "one"}, ExecuteFunc: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		panic("expected")
	}}
	registry.MustRegister(toolFunc)
	result, err := agent.New(&singleToolProvider{}, registry).Run(t.Context(), "run")
	if err != nil || result.Message.Content[0].(llm.TextBlock).Text != "done" {
		t.Fatalf("Run = %#v, %v", result, err)
	}
	if calls := toolFunc.Calls(); len(calls) != 1 {
		t.Fatalf("calls = %#v", calls)
	}
}

type singleToolProvider struct {
	mu    sync.Mutex
	calls int
}

func (*singleToolProvider) Name() string { return "single-tool" }
func (*singleToolProvider) Chat(context.Context, []llm.Message, []tool.ToolInfo, ...llm.Option) (*llm.Message, *llm.Usage, error) {
	return nil, nil, llm.ErrStreamingNotSupported
}
func (p *singleToolProvider) ChatStream(context.Context, []llm.Message, []tool.ToolInfo, ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	p.mu.Lock()
	p.calls++
	call := p.calls
	p.mu.Unlock()
	if call == 1 {
		return func(yield func(llm.Chunk, error) bool) {
			if !yield(llm.ToolCallStartChunk{ID: "one", Name: "one"}, nil) {
				return
			}
			yield(llm.ToolCallArgsChunk{ID: "one", Delta: "{}"}, nil)
		}, nil
	}
	return func(yield func(llm.Chunk, error) bool) { yield(llm.TextDeltaChunk{Text: "done"}, nil) }, nil
}
