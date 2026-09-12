package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/dailz1/go-agent/pkg/store"
	"github.com/dailz1/go-agent/pkg/tool"
)

func TestSerialToolHardErrorStopsAcceptedIterator(t *testing.T) {
	reg := tool.NewRegistry()
	first := &parallelTestTool{info: tool.ToolInfo{Name: "one"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult("one"), nil
	}}
	second := &parallelTestTool{info: tool.ToolInfo{Name: "two"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return nil, errors.New("boom")
	}}
	third := &parallelTestTool{info: tool.ToolInfo{Name: "three"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult("wrong"), nil
	}}
	reg.MustRegister(first)
	reg.MustRegister(second)
	reg.MustRegister(third)
	provider := parallelProvider(parallelToolCalls("one", "two", "three"))
	st := store.NewMemory()
	agent := New(provider, reg, WithStore(st), WithToolConcurrency(1), WithLogger(discardLogger()))
	seq, err := agent.RunThreadStream(context.Background(), "t", "go")
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	var errorsSeen []error
	for _, streamErr := range seq {
		if streamErr != nil {
			errorsSeen = append(errorsSeen, streamErr)
		}
	}
	if len(errorsSeen) != 1 || !containsAll(errorsSeen[0].Error(), "two", "execute: boom") {
		t.Fatalf("errors = %v, want one c2 hard error", errorsSeen)
	}
	if first.calls.Load() != 1 || second.calls.Load() != 1 || third.calls.Load() != 0 {
		t.Fatalf("tool calls = %d/%d/%d, want 1/1/0", first.calls.Load(), second.calls.Load(), third.calls.Load())
	}
	if provider.index != 1 {
		t.Errorf("provider calls = %d, want 1", provider.index)
	}
	if hasKind(t, st, "t", store.KindRoundCommitted) {
		t.Error("hard-error round committed")
	}
}
