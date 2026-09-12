package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/dailz1/go-agent/pkg/tool"
)

func TestParallelNoCallbackFailClosed(t *testing.T) {
	reg := tool.NewRegistry()
	noCallback := &parallelTestTool{info: tool.ToolInfo{Name: "approval", RequiresApproval: true}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult("wrong"), nil
	}}
	reg.MustRegister(noCallback)
	if _, err := New(parallelProvider(parallelToolCalls("approval")), reg, WithToolConcurrency(2), WithLogger(discardLogger())).Run(context.Background(), "go"); err != nil {
		t.Fatalf("no callback Run: %v", err)
	}
	if noCallback.calls.Load() != 0 {
		t.Errorf("no-callback tool executed %d times, want 0", noCallback.calls.Load())
	}
}
