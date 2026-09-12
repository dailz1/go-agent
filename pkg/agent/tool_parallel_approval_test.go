package agent

import (
	"context"
	"encoding/json"
	"iter"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/dailz1/go-agent/pkg/llm"
	"github.com/dailz1/go-agent/pkg/tool"
)

func TestParallelMixedApprovalPlanning(t *testing.T) {
	calls := parallelToolCalls("approved", "rejected", "missing_callback", "ghost")
	reg := tool.NewRegistry()
	approved := &parallelTestTool{info: tool.ToolInfo{Name: "approved", RequiresApproval: true}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) { return tool.NewTextResult("ok"), nil }}
	rejected := &parallelTestTool{info: tool.ToolInfo{Name: "rejected", RequiresApproval: true}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult("wrong"), nil
	}}
	missing := &parallelTestTool{info: tool.ToolInfo{Name: "missing_callback", RequiresApproval: true}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult("wrong"), nil
	}}
	reg.MustRegister(approved)
	reg.MustRegister(rejected)
	reg.MustRegister(missing)
	var approvalOrder []string
	result, err := New(parallelProvider(calls), reg, WithToolConcurrency(4), WithApprovalFn(func(info tool.ToolInfo, _ json.RawMessage) bool {
		approvalOrder = append(approvalOrder, info.Name)
		return info.Name == "approved"
	}), WithLogger(discardLogger())).Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if approved.calls.Load() != 1 || rejected.calls.Load() != 0 || missing.calls.Load() != 0 {
		t.Fatalf("execute counts = %d/%d/%d, want 1/0/0", approved.calls.Load(), rejected.calls.Load(), missing.calls.Load())
	}
	if !reflect.DeepEqual(approvalOrder, []string{"approved", "rejected", "missing_callback"}) {
		t.Errorf("approval order = %v, want declaration order", approvalOrder)
	}
	got := historyResultBlocks(result.History)
	if len(got) != 4 || !got[1].IsError || !got[2].IsError || !got[3].IsError {
		t.Fatalf("mixed planning results = %#v, want ordered soft errors", got)
	}

}

type plannerInfoPanicTool struct {
	armed     atomic.Bool
	execCalls atomic.Int32
}

func (t *plannerInfoPanicTool) Info() tool.ToolInfo {
	if t.armed.Load() {
		panic("info")
	}
	return tool.ToolInfo{Name: "panic_info"}
}
func (t *plannerInfoPanicTool) Execute(context.Context, json.RawMessage) (*tool.ToolResult, error) {
	t.execCalls.Add(1)
	return tool.NewTextResult("wrong"), nil
}

type plannerArmProvider struct {
	*MockStreamingProvider
	arm func()
}

func (p *plannerArmProvider) ChatStream(ctx context.Context, messages []llm.Message, tools []tool.ToolInfo, opts ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	p.arm()
	return p.MockStreamingProvider.ChatStream(ctx, messages, tools, opts...)
}

func TestParallelPlanFreezesRegistryAndRunToolsSnapshot(t *testing.T) {
	reg := tool.NewRegistry()
	late := &parallelTestTool{info: tool.ToolInfo{Name: "late"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult("wrong"), nil
	}}
	register := &parallelTestTool{info: tool.ToolInfo{Name: "register"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		if err := reg.Register(late); err != nil {
			return nil, err
		}
		return tool.NewTextResult("registered"), nil
	}}
	reg.MustRegister(register)
	provider := NewMockStreamingProvider([][]llm.Chunk{parallelToolRound(parallelToolCalls("register")), parallelToolRound(parallelToolCalls("late")), {llm.TextDeltaChunk{Text: "done"}, llm.DoneChunk{FinishReason: "stop"}}})
	result, err := New(provider, reg, WithToolConcurrency(2), WithLogger(discardLogger())).Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if late.calls.Load() != 1 {
		t.Fatalf("late tool executed %d times in later live lookup, want 1", late.calls.Load())
	}
	results := historyResultBlocks(result.History)
	if len(results) != 2 || results[0].ToolUseID != "c1" || results[1].ToolUseID != "c1" {
		t.Fatalf("later-round results = %#v, want register then late", results)
	}
	for _, info := range provider.LastTools {
		if info.Name == "late" {
			t.Fatal("model-visible tools refreshed after registry registration")
		}
	}
}
