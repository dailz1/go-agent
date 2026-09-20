package agenttool_test

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dailz1/go-agent/agent"
	"github.com/dailz1/go-agent/agenttest"
	"github.com/dailz1/go-agent/agenttool"
	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

var _ tool.Tool = agenttool.New(nil, agenttool.Config{Name: "worker", Description: "delegates work", RequiresApproval: true})

func TestNewInfo(t *testing.T) {
	adapter := agenttool.New(nil, agenttool.Config{Name: "worker", Description: "delegates work", RequiresApproval: true})
	info := adapter.Info()
	if info.Name != "worker" || info.Description != "delegates work" || !info.RequiresApproval || info.Parameters.Type != "object" || !reflect.DeepEqual(info.Parameters.Required, []string{"input"}) || info.Parameters.Properties["input"].Type != "string" {
		t.Fatalf("Info() = %#v", info)
	}
	registry := tool.NewRegistry()
	if err := registry.Register(adapter); err != nil || !reflect.DeepEqual(registry.List(), []tool.ToolInfo{info}) {
		t.Fatalf("Register/List = %v, %#v", err, registry.List())
	}
}

func TestMalformedArgumentsAreSoftAndDoNotRunChild(t *testing.T) {
	provider := &countingProvider{Provider: scripted("unused", nil, llm.TextDeltaChunk{Text: "unused"})}
	adapter := agenttool.New(agent.New(provider, tool.NewRegistry()), agenttool.Config{Name: "worker"})
	for _, args := range []json.RawMessage{nil, []byte(`null`), []byte(`[]`), []byte(`"input"`), []byte(`{}`), []byte(`{"input":null}`), []byte(`{"input":1}`), []byte(`{`)} {
		result, err := adapter.Execute(t.Context(), args)
		if err != nil || result == nil || !result.IsError() {
			t.Fatalf("Execute(%s) = %#v, %v", args, result, err)
		}
	}
	if provider.calls.Load() != 0 {
		t.Fatalf("child provider calls = %d, want 0", provider.calls.Load())
	}
}

func TestSubAgentHardErrorReachesParent(t *testing.T) {
	sentinel := errors.New("child unavailable")
	childProvider := agenttest.NewScriptedProvider(agenttest.Exchange{Method: agenttest.MethodChatStream, Request: request("task", nil), StreamOuterErr: sentinel})
	adapter := agenttool.New(agent.New(childProvider, tool.NewRegistry()), agenttool.Config{Name: "worker"})
	registry := tool.NewRegistry()
	registry.MustRegister(adapter)
	_, err := agent.New(&callProvider{name: "worker", args: `{"input":"task"}`}, registry).Run(t.Context(), "parent task")
	if !errors.Is(err, sentinel) || err == nil || !strings.Contains(err.Error(), `run sub-agent "worker"`) {
		t.Fatalf("parent Run error = %v", err)
	}
	if err := childProvider.Verify(); err != nil {
		t.Fatal(err)
	}
}

func TestApprovalBoundariesRemainIndependent(t *testing.T) {
	t.Run("outer", func(t *testing.T) {
		childProvider := &countingProvider{Provider: scripted("task", nil, llm.TextDeltaChunk{Text: "unused"})}
		adapter := agenttool.New(agent.New(childProvider, tool.NewRegistry()), agenttool.Config{Name: "worker", RequiresApproval: true})
		info, args := adapter.Info(), json.RawMessage(`{"input":"task"}`)
		parentProvider := agenttest.NewScriptedProvider(
			agenttest.Exchange{Method: agenttest.MethodChatStream, Request: request("parent", []tool.ToolInfo{info}), StreamChunks: []llm.Chunk{llm.ToolCallStartChunk{ID: "call", Name: "worker"}, llm.ToolCallArgsChunk{ID: "call", Delta: string(args)}}},
			agenttest.Exchange{Method: agenttest.MethodChatStream, Request: agenttest.Request{Messages: []llm.Message{llm.UserMessage("parent"), llm.AssistantToolCallMessage(llm.ToolUseBlock{ID: "call", Name: "worker", Input: args}), llm.ToolResultMessage("call", tool.NewErrorResult("tool %q requires approval but no approval callback is configured", "worker"))}, Tools: []tool.ToolInfo{info}}, StreamChunks: []llm.Chunk{llm.TextDeltaChunk{Text: "parent final"}}},
		)
		registry := tool.NewRegistry()
		registry.MustRegister(adapter)
		if _, err := agent.New(parentProvider, registry).Run(t.Context(), "parent"); err != nil || childProvider.calls.Load() != 0 {
			t.Fatalf("parent Run = %v; child calls = %d", err, childProvider.calls.Load())
		}
		if err := parentProvider.Verify(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("child", func(t *testing.T) {
		guarded := &agenttest.ToolFunc{Definition: tool.ToolInfo{Name: "guarded", RequiresApproval: true}, ExecuteFunc: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
			return tool.NewTextResult("wrong"), nil
		}}
		registry := tool.NewRegistry()
		registry.MustRegister(guarded)
		args, info := json.RawMessage(`{}`), guarded.Info()
		provider := agenttest.NewScriptedProvider(
			agenttest.Exchange{Method: agenttest.MethodChatStream, Request: request("task", []tool.ToolInfo{info}), StreamChunks: []llm.Chunk{llm.ToolCallStartChunk{ID: "call", Name: "guarded"}, llm.ToolCallArgsChunk{ID: "call", Delta: string(args)}}},
			agenttest.Exchange{Method: agenttest.MethodChatStream, Request: agenttest.Request{Messages: []llm.Message{llm.UserMessage("task"), llm.AssistantToolCallMessage(llm.ToolUseBlock{ID: "call", Name: "guarded", Input: args}), llm.ToolResultMessage("call", tool.NewErrorResult("tool %q requires approval but no approval callback is configured", "guarded"))}, Tools: []tool.ToolInfo{info}}, StreamChunks: []llm.Chunk{llm.TextDeltaChunk{Text: "child final"}}},
		)
		result, err := agenttool.New(agent.New(provider, registry), agenttool.Config{Name: "worker"}).Execute(t.Context(), []byte(`{"input":"task"}`))
		if err != nil || result.IsError() || result.Content != "child final" || len(guarded.Calls()) != 0 {
			t.Fatalf("Execute = %#v, %v; child calls = %#v", result, err, guarded.Calls())
		}
		if err := provider.Verify(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestTruncatedAndTextlessChildrenAreSoft(t *testing.T) {
	t.Run("truncated", func(t *testing.T) {
		childTool := &agenttest.ToolFunc{Definition: tool.ToolInfo{Name: "child-tool"}, ExecuteFunc: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
			return tool.NewTextResult("unused"), nil
		}}
		registry := tool.NewRegistry()
		registry.MustRegister(childTool)
		provider := scripted("task", []tool.ToolInfo{childTool.Info()}, llm.ToolCallStartChunk{ID: "child-call", Name: "child-tool"}, llm.ToolCallArgsChunk{ID: "child-call", Delta: `{}`})
		result, err := agenttool.New(agent.New(provider, registry, agent.WithMaxIter(1)), agenttool.Config{Name: "worker"}).Execute(t.Context(), []byte(`{"input":"task"}`))
		if err != nil || !result.IsError() || len(childTool.Calls()) != 0 {
			t.Fatalf("Execute = %#v, %v; child calls = %#v", result, err, childTool.Calls())
		}
	})
	t.Run("no text", func(t *testing.T) {
		result, err := agenttool.New(agent.New(scripted("task", nil, llm.ReasoningDeltaChunk{Text: "private"}), tool.NewRegistry()), agenttool.Config{Name: "worker"}).Execute(t.Context(), []byte(`{"input":"task"}`))
		if err != nil || !result.IsError() || result.Data != nil {
			t.Fatalf("Execute = %#v, %v", result, err)
		}
	})
}

type countingProvider struct {
	llm.Provider
	calls atomic.Int32
}

func (p *countingProvider) Chat(ctx context.Context, messages []llm.Message, tools []tool.ToolInfo, opts ...llm.Option) (*llm.Message, *llm.Usage, error) {
	p.calls.Add(1)
	return p.Provider.Chat(ctx, messages, tools, opts...)
}

func (p *countingProvider) ChatStream(ctx context.Context, messages []llm.Message, tools []tool.ToolInfo, opts ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	p.calls.Add(1)
	return p.Provider.ChatStream(ctx, messages, tools, opts...)
}

type callProvider struct{ name, args string }

func (*callProvider) Name() string { return "call" }
func (*callProvider) Chat(context.Context, []llm.Message, []tool.ToolInfo, ...llm.Option) (*llm.Message, *llm.Usage, error) {
	return nil, nil, llm.ErrStreamingNotSupported
}
func (p *callProvider) ChatStream(context.Context, []llm.Message, []tool.ToolInfo, ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	return chunks(llm.ToolCallStartChunk{ID: "parent-call", Name: p.name}, llm.ToolCallArgsChunk{ID: "parent-call", Delta: p.args}), nil
}

func scripted(input string, tools []tool.ToolInfo, chunks ...llm.Chunk) *agenttest.ScriptedProvider {
	return agenttest.NewScriptedProvider(agenttest.Exchange{Method: agenttest.MethodChatStream, Request: request(input, tools), StreamChunks: chunks})
}

func request(input string, tools []tool.ToolInfo) agenttest.Request {
	if tools == nil {
		tools = []tool.ToolInfo{}
	}
	return agenttest.Request{Messages: []llm.Message{llm.UserMessage(input)}, Tools: tools}
}

func chunks(values ...llm.Chunk) iter.Seq2[llm.Chunk, error] {
	return func(yield func(llm.Chunk, error) bool) {
		for _, value := range values {
			if !yield(value, nil) {
				return
			}
		}
	}
}
