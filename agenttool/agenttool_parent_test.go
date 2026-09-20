package agenttool_test

import (
	"context"
	"encoding/json"
	"iter"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"github.com/dailz1/go-agent/agent"
	"github.com/dailz1/go-agent/agenttest"
	"github.com/dailz1/go-agent/agenttool"
	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

func TestParentDelegationReturnsFinalTextAndOnlyParentEvents(t *testing.T) {
	cfg := agenttool.Config{Name: "worker", Description: "delegates work"}
	childProvider := scripted("child task", nil,
		llm.TextDeltaChunk{Text: "child ", OutputIndex: 0},
		llm.ReasoningItemChunk{OutputIndex: 1, Item: llm.ReasoningItemBlock{Type: "reasoning_item", ID: "private"}},
		llm.TextDeltaChunk{Text: "final", OutputIndex: 2},
	)
	adapter := agenttool.New(agent.New(childProvider, tool.NewRegistry()), cfg)
	info, args := adapter.Info(), json.RawMessage(`{"input":"child task"}`)
	parentProvider := agenttest.NewScriptedProvider(
		agenttest.Exchange{Method: agenttest.MethodChatStream, Request: request("parent task", []tool.ToolInfo{info}), StreamChunks: toolCallChunks([]llm.ToolUseBlock{{ID: "parent-call", Name: "worker", Input: args}})},
		agenttest.Exchange{Method: agenttest.MethodChatStream, Request: agenttest.Request{Messages: []llm.Message{llm.UserMessage("parent task"), llm.AssistantToolCallMessage(llm.ToolUseBlock{ID: "parent-call", Name: "worker", Input: args}), llm.ToolResultMessage("parent-call", tool.NewTextResult("child final"))}, Tools: []tool.ToolInfo{info}}, StreamChunks: []llm.Chunk{llm.TextDeltaChunk{Text: "parent final"}}},
	)
	registry := tool.NewRegistry()
	registry.MustRegister(adapter)
	seq, err := agent.New(parentProvider, registry).RunStream(t.Context(), "parent task")
	if err != nil {
		t.Fatal(err)
	}
	var events []agent.AgentEvent
	for event, err := range seq {
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	if len(events) != 4 {
		t.Fatalf("events = %#v", events)
	}
	call, ok := events[0].(agent.ToolCallEvent)
	if !ok || call.ID != "parent-call" || call.Name != "worker" || string(call.Args) != string(args) {
		t.Fatalf("parent ToolCallEvent = %#v", events[0])
	}
	result, ok := events[1].(agent.ToolResultEvent)
	if !ok || result.Name != "worker" || result.Result.Content != "child final" || result.Result.Data != nil {
		t.Fatalf("tool result = %#v", events[1])
	}
	if text, ok := events[2].(agent.TextDeltaEvent); !ok || text.Text != "parent final" {
		t.Fatalf("parent event = %#v", events[2])
	}
	done, ok := events[3].(agent.DoneEvent)
	if !ok {
		t.Fatalf("done event = %#v", events[3])
	}
	if folded := foldParentEvents(t, "parent task", events); !reflect.DeepEqual(folded, done.History) {
		t.Fatalf("folded history = %#v, want %#v", folded, done.History)
	}
	if err := childProvider.Verify(); err != nil {
		t.Fatal(err)
	}
	if err := parentProvider.Verify(); err != nil {
		t.Fatal(err)
	}
}

func TestParentTruncatesChildToolResult(t *testing.T) {
	content := strings.Repeat("x", 300)
	childProvider := scripted("task", nil, llm.TextDeltaChunk{Text: content})
	adapter := agenttool.New(agent.New(childProvider, tool.NewRegistry()), agenttool.Config{Name: "worker"})
	registry := tool.NewRegistry()
	registry.MustRegister(adapter)
	parent := agent.New(&toolCallsProvider{calls: []llm.ToolUseBlock{{ID: "call", Name: "worker", Input: json.RawMessage(`{"input":"task"}`)}}}, registry, agent.WithContextWindowTokens(128), agent.WithCompactor(nil))
	seq, err := parent.RunStream(t.Context(), "parent")
	if err != nil {
		t.Fatal(err)
	}
	var result *tool.ToolResult
	for event, err := range seq {
		if err != nil {
			t.Fatal(err)
		}
		if event, ok := event.(agent.ToolResultEvent); ok {
			result = event.Result
		}
	}
	if result == nil || utf8.RuneCountInString(result.Content) != 128 || !strings.Contains(result.Content, "tool result truncated") || result.Data != nil {
		t.Fatalf("parent ToolResult = %#v", result)
	}
	if err := childProvider.Verify(); err != nil {
		t.Fatal(err)
	}
}

func foldParentEvents(t *testing.T, input string, events []agent.AgentEvent) []llm.Message {
	t.Helper()
	history := []llm.Message{llm.UserMessage(input)}
	var calls []llm.ToolUseBlock
	var text strings.Builder
	for _, event := range events {
		switch event := event.(type) {
		case agent.ToolCallEvent:
			calls = append(calls, llm.ToolUseBlock{Type: "tool_use", ID: event.ID, Name: event.Name, Input: event.Args})
		case agent.ToolResultEvent:
			if len(calls) == 0 {
				t.Fatal("ToolResultEvent without a ToolCallEvent")
			}
			history = append(history, llm.AssistantToolCallMessage(calls...), llm.ToolResultMessage(event.ID, event.Result))
			calls = nil
		case agent.TextDeltaEvent:
			text.WriteString(event.Text)
		case agent.DoneEvent:
			if len(calls) != 0 {
				t.Fatal("DoneEvent with unpaired ToolCallEvent")
			}
			history = append(history, llm.AssistantMessage(text.String()))
		default:
			t.Fatalf("unexpected parent event %T", event)
		}
	}
	return history
}

type toolCallsProvider struct {
	calls  []llm.ToolUseBlock
	rounds atomic.Int32
}

func (*toolCallsProvider) Name() string { return "parent-calls" }
func (*toolCallsProvider) Chat(context.Context, []llm.Message, []tool.ToolInfo, ...llm.Option) (*llm.Message, *llm.Usage, error) {
	return nil, nil, llm.ErrStreamingNotSupported
}
func (p *toolCallsProvider) ChatStream(context.Context, []llm.Message, []tool.ToolInfo, ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	if p.rounds.Add(1) == 1 {
		return chunks(toolCallChunks(p.calls)...), nil
	}
	return chunks(llm.TextDeltaChunk{Text: "parent final"}), nil
}

func toolCallChunks(calls []llm.ToolUseBlock) []llm.Chunk {
	chunks := make([]llm.Chunk, 0, len(calls)*2)
	for index, call := range calls {
		chunks = append(chunks, llm.ToolCallStartChunk{Index: index, ID: call.ID, Name: call.Name}, llm.ToolCallArgsChunk{Index: index, ID: call.ID, Delta: string(call.Input)})
	}
	return chunks
}
