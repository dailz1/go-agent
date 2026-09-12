package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/dailz1/go-agent/pkg/tool"
)

func TestParallelCancelAfterAnnouncements(t *testing.T) {
	calls := parallelToolCalls("one", "two")
	reg := tool.NewRegistry()
	one := &parallelTestTool{info: tool.ToolInfo{Name: "one"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult("one"), nil
	}}
	two := &parallelTestTool{info: tool.ToolInfo{Name: "two"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult("two"), nil
	}}
	reg.MustRegister(one)
	reg.MustRegister(two)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	seq, err := New(parallelProvider(calls), reg, WithToolConcurrency(2), WithLogger(discardLogger())).RunStream(ctx, "go")
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	var terminal error
	for event, streamErr := range seq {
		if streamErr != nil {
			terminal = streamErr
			break
		}
		if call, ok := event.(ToolCallEvent); ok && call.ID == "c2" {
			cancel()
		}
	}
	if !errors.Is(terminal, context.Canceled) || one.calls.Load() != 0 || two.calls.Load() != 0 {
		t.Fatalf("cancel result = %v, calls = %d/%d; want canceled and 0/0", terminal, one.calls.Load(), two.calls.Load())
	}
}

func TestParallelConsumerBreakDuringAnnouncements(t *testing.T) {
	reg := tool.NewRegistry()
	breakTool := &parallelTestTool{info: tool.ToolInfo{Name: "break"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult("wrong"), nil
	}}
	reg.MustRegister(breakTool)
	seq, err := New(parallelProvider(parallelToolCalls("break")), reg, WithToolConcurrency(2), WithLogger(discardLogger())).RunStream(context.Background(), "go")
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	for event, streamErr := range seq {
		if streamErr != nil {
			t.Fatalf("stream: %v", streamErr)
		}
		if _, ok := event.(ToolCallEvent); ok {
			break
		}
	}
	if breakTool.calls.Load() != 0 {
		t.Errorf("tool ran after consumer broke ToolCall batch: %d", breakTool.calls.Load())
	}
}
