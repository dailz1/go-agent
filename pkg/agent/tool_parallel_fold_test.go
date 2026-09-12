package agent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/dailz1/go-agent/pkg/llm"
	"github.com/dailz1/go-agent/pkg/tool"
)

func TestParallelEventsFoldToDoneHistory(t *testing.T) {
	reg := tool.NewRegistry()
	for _, name := range []string{"one", "two"} {
		name := name
		reg.MustRegister(&parallelTestTool{info: tool.ToolInfo{Name: name}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
			return tool.NewTextResult(name), nil
		}})
	}
	seq, err := New(parallelProvider(parallelToolCalls("one", "two")), reg, WithToolConcurrency(2), WithLogger(discardLogger())).RunStream(context.Background(), "go")
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	fold := newEventFold([]llm.Message{llm.UserMessage("go")})
	var done DoneEvent
	var resultPairs []string
	for event, streamErr := range seq {
		if streamErr != nil {
			t.Fatalf("stream: %v", streamErr)
		}
		fold.apply(t, event)
		if result, ok := event.(ToolResultEvent); ok {
			resultPairs = append(resultPairs, result.ID+":"+result.Result.Content)
		}
		if terminal, ok := event.(DoneEvent); ok {
			done = terminal
		}
	}
	if !reflect.DeepEqual(resultPairs, []string{"c1:one", "c2:two"}) {
		t.Fatalf("result events = %v, want declaration-order content pairs", resultPairs)
	}
	if !reflect.DeepEqual(fold.full(), done.History) {
		t.Fatalf("folded history = %#v, want %#v", fold.full(), done.History)
	}
}

func TestParallelHardErrorFoldsOrderedPrefix(t *testing.T) {
	calls := parallelToolCalls("one", "two")
	reg := tool.NewRegistry()
	reg.MustRegister(&parallelTestTool{info: tool.ToolInfo{Name: "one"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult("one"), nil
	}})
	reg.MustRegister(&parallelTestTool{info: tool.ToolInfo{Name: "two"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return nil, errors.New("boom")
	}})
	seq, err := New(parallelProvider(calls), reg, WithToolConcurrency(2), WithLogger(discardLogger())).RunStream(context.Background(), "go")
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	fold := newEventFold([]llm.Message{llm.UserMessage("go")})
	for event := range seq {
		if event != nil {
			fold.apply(t, event)
		}
	}
	want := []llm.Message{
		llm.UserMessage("go"),
		llm.AssistantToolCallMessage(calls...),
		llm.ToolResultMessage("c1", tool.NewTextResult("one")),
	}
	if !reflect.DeepEqual(fold.finishErr(), want) {
		t.Fatalf("folded error history = %#v, want %#v", fold.finishErr(), want)
	}
}
