package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

type noChangeParallelCompactor struct{}

func (noChangeParallelCompactor) Compact(context.Context, []llm.Message, CompactionBudget) (CompactionResult, error) {
	return CompactionResult{}, nil
}

func TestParallelCompactionBudgetFailsClosed(t *testing.T) {
	reg := tool.NewRegistry()
	reg.MustRegister(&parallelTestTool{info: tool.ToolInfo{Name: "large"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult(strings.Repeat("x", 128)), nil
	}})
	_, err := New(parallelProvider(parallelToolCalls("large")), reg, WithToolConcurrency(2),
		WithContextWindowTokens(128), WithCompactor(noChangeParallelCompactor{}), WithLogger(discardLogger())).Run(context.Background(), "go")
	if !errors.Is(err, ErrCompactionBudgetExceeded) {
		t.Fatalf("Run error = %v, want ErrCompactionBudgetExceeded", err)
	}
}
