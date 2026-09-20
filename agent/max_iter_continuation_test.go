package agent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

func terminalHistory(t *testing.T) []llm.Message {
	t.Helper()
	registry := tool.NewRegistry()
	registry.MustRegister(&mockTool{info: tool.ToolInfo{Name: "echo"}, result: tool.NewTextResult("must not run")})
	result, err := New(
		NewMockProvider(MsgResponse(llm.AssistantToolCallMessage(
			llm.ToolUseBlock{Type: "tool_use", ID: "skip", Name: "echo", Input: json.RawMessage(`{}`)},
		))),
		registry,
		WithMaxIter(1),
		WithLogger(discardLogger()),
	).Run(context.Background(), "first")
	if err != nil {
		t.Fatalf("terminal Run: %v", err)
	}
	return result.History
}

func TestMaxIterNonPersistentContinuationUsesPairedHistory(t *testing.T) {
	history := terminalHistory(t)
	provider := NewMockProvider(MsgResponse(llm.AssistantMessage("continued")))
	result, err := New(provider, tool.NewRegistry(), WithLogger(discardLogger())).RunWithHistory(context.Background(), history, "next")
	if err != nil {
		t.Fatalf("RunWithHistory: %v", err)
	}
	wantInput := append(deepCopyMessages(history), llm.UserMessage("next"))
	if !reflect.DeepEqual(provider.LastMessages, wantInput) {
		t.Errorf("provider input = %#v, want %#v", provider.LastMessages, wantInput)
	}
	if result.Truncated || result.ToolCalls != 0 {
		t.Errorf("continuation result = %#v, want normal no-tool completion", result)
	}
}

func TestMaxIterStoreConfiguredHistoryEntrypointsStayNonPersistent(t *testing.T) {
	history := terminalHistory(t)
	for _, test := range []struct {
		name string
		run  func(*Agent, []llm.Message) (*RunResult, error)
	}{
		{
			name: "RunWithHistory",
			run: func(agent *Agent, input []llm.Message) (*RunResult, error) {
				return agent.RunWithHistory(context.Background(), input, "next")
			},
		},
		{
			name: "RunStreamWithHistory",
			run: func(agent *Agent, input []llm.Message) (*RunResult, error) {
				seq, err := agent.RunStreamWithHistory(context.Background(), input, "next")
				if err != nil {
					return nil, err
				}
				return agent.foldRunStream(seq)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			st := store.NewMemory()
			provider := NewMockProvider(MsgResponse(llm.AssistantMessage("continued")))
			agent := New(provider, tool.NewRegistry(), WithStore(st), WithLogger(discardLogger()))
			result, err := test.run(agent, history)
			if err != nil {
				t.Fatalf("%s: %v", test.name, err)
			}
			if result.ThreadID != "" {
				t.Errorf("ThreadID = %q, want empty", result.ThreadID)
			}
			state, err := st.Latest(context.Background(), "unused")
			if err != nil {
				t.Fatalf("Latest: %v", err)
			}
			if state.Head != 0 {
				t.Errorf("store head = %d, want 0", state.Head)
			}
			want := append(deepCopyMessages(history), llm.UserMessage("next"))
			if !reflect.DeepEqual(provider.LastMessages, want) {
				t.Errorf("provider input = %#v, want %#v", provider.LastMessages, want)
			}
		})
	}
}

func TestMaxIterPersistentContinuationIsClosed(t *testing.T) {
	st := store.NewMemory()
	registry := tool.NewRegistry()
	registry.MustRegister(&mockTool{info: tool.ToolInfo{Name: "echo"}, result: tool.NewTextResult("must not run")})
	agent := New(
		NewMockProvider(
			MsgResponse(toolCallMessage("skip")),
			MsgResponse(llm.AssistantMessage("continued")),
		),
		registry,
		WithStore(st),
		WithMaxIter(1),
		WithLogger(discardLogger()),
	)
	first, err := agent.RunThread(context.Background(), "thread", "first")
	if err != nil {
		t.Fatalf("first RunThread: %v", err)
	}
	if !first.Truncated || len(historyResultBlocks(first.History)) != 1 {
		t.Fatalf("first result = %#v, want paired truncated history", first)
	}
	if _, err := agent.ResumeThread(context.Background(), "thread"); !errors.Is(err, ErrNothingToResume) {
		t.Errorf("ResumeThread = %v, want ErrNothingToResume", err)
	}
	if _, err := agent.RunThread(context.Background(), "thread", "next"); err != nil {
		t.Fatalf("next RunThread: %v", err)
	}
	input := agent.provider.(*MockProvider).LastMessages
	if len(input) < 4 || input[1].Role != llm.RoleAssistant || input[2].Role != llm.RoleTool {
		t.Errorf("persistent continuation input = %#v, want paired terminal call/result", input)
	}
}
