package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/dailz1/go-agent/tool"
)

func TestParallelExecutePanicIsSoftResult(t *testing.T) {
	reg := tool.NewRegistry()
	reg.MustRegister(&parallelTestTool{info: tool.ToolInfo{Name: "panic"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		panic("execute")
	}})
	result, err := New(parallelProvider(parallelToolCalls("panic")), reg, WithToolConcurrency(2), WithLogger(discardLogger())).Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	results := historyResultBlocks(result.History)
	if len(results) != 1 || !results[0].IsError || !contains(results[0].Content, "panicked: execute") {
		t.Fatalf("panic result = %#v, want soft panic error", results)
	}
}
