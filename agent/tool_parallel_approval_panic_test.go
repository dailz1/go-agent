package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/dailz1/go-agent/tool"
)

func TestParallelApprovalCallbackPanicZeroExecute(t *testing.T) {
	panicReg := tool.NewRegistry()
	panicTool := &parallelTestTool{info: tool.ToolInfo{Name: "panic", RequiresApproval: true}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult("wrong"), nil
	}}
	panicReg.MustRegister(panicTool)
	seq, err := New(parallelProvider(parallelToolCalls("panic")), panicReg, WithToolConcurrency(2), WithApprovalFn(func(tool.ToolInfo, json.RawMessage) bool { panic("approval") }), WithLogger(discardLogger())).RunStream(context.Background(), "go")
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	_, errs := collectEvents(t, seq, nil)
	if len(errs) != 1 || !containsAll(errs[0].Error(), "approval callback panicked", "panic") {
		t.Fatalf("callback panic errors = %v", errs)
	}
	if panicTool.calls.Load() != 0 {
		t.Errorf("callback panic executed %d tools, want 0", panicTool.calls.Load())
	}
}
