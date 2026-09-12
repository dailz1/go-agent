package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dailz1/go-agent/pkg/store"
	"github.com/dailz1/go-agent/pkg/tool"
)

func TestMaxIterPersistent(t *testing.T) {
	var executed atomic.Int32
	reg := tool.NewRegistry()
	reg.MustRegister(&parallelTestTool{info: tool.ToolInfo{Name: "one"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		executed.Add(1)
		return tool.NewTextResult("one"), nil
	}})
	st := store.NewMemory()
	result, err := New(parallelProvider(parallelToolCalls("one")), reg, WithStore(st), WithToolConcurrency(2), WithMaxIter(1), WithLogger(discardLogger())).RunThread(context.Background(), "limit", "go")
	if err != nil || !result.Truncated || executed.Load() != 0 {
		t.Fatalf("persistent maxIter result = %#v, err = %v, executions = %d", result, err, executed.Load())
	}
	var committed *roundCommittedPayload
	for _, record := range logRecords(t, st, "limit") {
		if record.Kind == store.KindRoundCommitted {
			decoded := new(roundCommittedPayload)
			if err := strictDecode(record.Payload, decoded); err != nil {
				t.Fatal(err)
			}
			committed = decoded
		}
	}
	if committed == nil {
		t.Fatal("persistent maxIter did not commit its skipped result")
	}
	if len(committed.Results) != 1 || !strings.Contains(resultBlocks(committed.Results[0])[0].Content, "not executed") {
		t.Fatalf("maxIter committed results = %#v", committed.Results)
	}
}

func TestMaxIterNonPersistent(t *testing.T) {
	var executed atomic.Int32
	reg := tool.NewRegistry()
	reg.MustRegister(&parallelTestTool{info: tool.ToolInfo{Name: "one"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		executed.Add(1)
		return tool.NewTextResult("one"), nil
	}})
	result, err := New(parallelProvider(parallelToolCalls("one")), reg, WithToolConcurrency(2), WithMaxIter(1), WithLogger(discardLogger())).Run(context.Background(), "go")
	if err != nil || !result.Truncated || executed.Load() != 0 {
		t.Fatalf("non-persistent maxIter result = %#v, err = %v, executions = %d", result, err, executed.Load())
	}
	if len(historyResultBlocks(result.History)) != 0 {
		t.Fatalf("non-persistent maxIter history synthesized results: %#v", result.History)
	}
}
