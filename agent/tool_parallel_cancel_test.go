package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/dailz1/go-agent/tool"
)

func TestParallelPlannerInfoPanicZeroExecute(t *testing.T) {
	panicTool := &plannerInfoPanicTool{}
	first := &parallelTestTool{info: tool.ToolInfo{Name: "one"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult("wrong"), nil
	}}
	third := &parallelTestTool{info: tool.ToolInfo{Name: "three"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult("wrong"), nil
	}}
	reg := tool.NewRegistry()
	reg.MustRegister(first)
	reg.MustRegister(panicTool)
	reg.MustRegister(third)
	provider := &plannerArmProvider{MockStreamingProvider: parallelProvider(parallelToolCalls("one", "panic_info", "three")), arm: func() { panicTool.armed.Store(true) }}
	seq, err := New(provider, reg, WithToolConcurrency(2), WithLogger(discardLogger())).RunStream(context.Background(), "go")
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	_, errs := collectEvents(t, seq, nil)
	if len(errs) != 1 || !containsAll(errs[0].Error(), "tool info panicked", "panic_info") || panicTool.execCalls.Load() != 0 || first.calls.Load() != 0 || third.calls.Load() != 0 {
		t.Fatalf("Info panic errors = %v, execute calls = %d/%d/%d", errs, first.calls.Load(), panicTool.execCalls.Load(), third.calls.Load())
	}
}
