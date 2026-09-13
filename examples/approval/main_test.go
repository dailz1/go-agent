package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/dailz1/go-agent/pkg/agent"
	"github.com/dailz1/go-agent/pkg/tool"
)

func TestApprovalAllowAndRejectAreDeterministic(t *testing.T) {
	for _, allow := range []bool{false, true} {
		called := false
		registry := tool.NewRegistry()
		registry.MustRegister(&countingTool{called: &called})
		result, err := agent.New(&approvalProvider{}, registry, agent.WithApprovalFn(func(tool.ToolInfo, json.RawMessage) bool {
			return allow
		})).Run(context.Background(), "x")
		if err != nil {
			t.Fatalf("run(%v): %v", allow, err)
		}
		if result.ToolCalls != 1 || called != allow {
			t.Fatalf("allow=%v called=%v toolCalls=%d", allow, called, result.ToolCalls)
		}
	}
}

func TestMissingApprovalCallbackRejectsWithoutExecuting(t *testing.T) {
	called := false
	registry := tool.NewRegistry()
	registry.MustRegister(&countingTool{called: &called})
	result, err := agent.New(&approvalProvider{}, registry).Run(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	if called || result.ToolCalls != 1 {
		t.Fatalf("called=%v toolCalls=%d, want rejected call", called, result.ToolCalls)
	}
}

type countingTool struct{ called *bool }

func (*countingTool) Info() tool.ToolInfo {
	return tool.ToolInfo{Name: "write_note", Parameters: tool.NewParameterSchema(), RequiresApproval: true}
}
func (t *countingTool) Execute(context.Context, json.RawMessage) (*tool.ToolResult, error) {
	*t.called = true
	return tool.NewTextResult("unexpected"), nil
}
