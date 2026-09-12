package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/dailz1/go-agent/pkg/tool"
)

func TestSerialLiveGetFindsToolRegisteredEarlierInRound(t *testing.T) {
	reg := tool.NewRegistry()
	late := &parallelTestTool{info: tool.ToolInfo{Name: "late"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult("late"), nil
	}}
	reg.MustRegister(&parallelTestTool{info: tool.ToolInfo{Name: "register"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		if err := reg.Register(late); err != nil {
			return nil, err
		}
		return tool.NewTextResult("registered"), nil
	}})
	result, err := New(parallelProvider(parallelToolCalls("register", "late")), reg, WithToolConcurrency(1), WithLogger(discardLogger())).Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if late.calls.Load() != 1 || len(historyResultBlocks(result.History)) != 2 || historyResultBlocks(result.History)[1].IsError {
		t.Fatalf("serial live lookup result = %#v, late calls = %d", result, late.calls.Load())
	}
}
