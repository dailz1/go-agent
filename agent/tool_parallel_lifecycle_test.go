package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

func TestParallelDeclarationIsDurableBeforeApproval(t *testing.T) {
	st := store.NewMemory()
	reg := tool.NewRegistry()
	reg.MustRegister(&parallelTestTool{info: tool.ToolInfo{Name: "approval", RequiresApproval: true}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult("ok"), nil
	}})
	approvedAfterDeclaration := false
	agent := New(parallelProvider(parallelToolCalls("approval")), reg, WithStore(st), WithToolConcurrency(2),
		WithApprovalFn(func(tool.ToolInfo, json.RawMessage) bool {
			approvedAfterDeclaration = hasKind(t, st, "t", store.KindRoundDeclared)
			return true
		}), WithLogger(discardLogger()))
	if _, err := agent.RunThread(context.Background(), "t", "go"); err != nil {
		t.Fatalf("RunThread: %v", err)
	}
	if !approvedAfterDeclaration {
		t.Fatal("approval ran before the declaration was durable")
	}
}
