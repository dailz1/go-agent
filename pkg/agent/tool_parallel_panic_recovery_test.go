package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/dailz1/go-agent/pkg/store"
	"github.com/dailz1/go-agent/pkg/tool"
)

func TestParallelApprovalPanicLeavesRecoverableDeclaration(t *testing.T) {
	st := store.NewMemory()
	reg := tool.NewRegistry()
	first := &parallelTestTool{info: tool.ToolInfo{Name: "one"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult("wrong"), nil
	}}
	second := &parallelTestTool{info: tool.ToolInfo{Name: "two", RequiresApproval: true}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult("wrong"), nil
	}}
	reg.MustRegister(first)
	reg.MustRegister(second)
	agent := New(parallelProvider(parallelToolCalls("one", "two")), reg, WithStore(st), WithToolConcurrency(2), WithApprovalFn(func(tool.ToolInfo, json.RawMessage) bool { panic("approval") }), WithLogger(discardLogger()))
	if _, err := agent.RunThread(context.Background(), "t", "go"); err == nil {
		t.Fatal("RunThread accepted callback panic")
	}
	if first.calls.Load() != 0 || second.calls.Load() != 0 || hasKind(t, st, "t", store.KindRoundCommitted) {
		t.Fatal("callback panic executed or committed its round")
	}
	result, err := agent.ResumeThread(context.Background(), "t")
	if err != nil {
		t.Fatalf("ResumeThread: %v", err)
	}
	unknown := historyResultBlocks(result.History)
	if len(unknown) < 2 || unknown[0].ToolUseID != "c1" || unknown[1].ToolUseID != "c2" {
		t.Fatalf("recovery results = %#v", unknown)
	}
}
