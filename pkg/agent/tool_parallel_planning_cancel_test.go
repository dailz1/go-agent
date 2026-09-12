package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/dailz1/go-agent/pkg/tool"
)

func TestParallelCancelDuringPlanning(t *testing.T) {
	first := &parallelTestTool{info: tool.ToolInfo{Name: "one", RequiresApproval: true}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult("wrong"), nil
	}}
	second := &parallelTestTool{info: tool.ToolInfo{Name: "two"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult("wrong"), nil
	}}
	reg := tool.NewRegistry()
	reg.MustRegister(first)
	reg.MustRegister(second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	agent := New(parallelProvider(parallelToolCalls("one", "two")), reg, WithToolConcurrency(2),
		WithApprovalFn(func(tool.ToolInfo, json.RawMessage) bool { cancel(); return true }), WithLogger(discardLogger()))
	seq, err := agent.RunStream(ctx, "go")
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	_, errs := collectEvents(t, seq, nil)
	if len(errs) != 1 || !errors.Is(errs[0], context.Canceled) {
		t.Fatalf("planning cancellation errors = %v", errs)
	}
	if first.calls.Load() != 0 || second.calls.Load() != 0 {
		t.Fatalf("planned tool calls = %d/%d, want 0/0", first.calls.Load(), second.calls.Load())
	}
}
