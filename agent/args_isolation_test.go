package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

// recordingTool captures the raw input bytes each invocation receives, so
// tests can compare what the kernel executed against what events showed.
type recordingTool struct {
	info   tool.ToolInfo
	result *tool.ToolResult
	inputs []json.RawMessage
}

func (t *recordingTool) Info() tool.ToolInfo { return t.info }

func (t *recordingTool) Execute(_ context.Context, input json.RawMessage) (*tool.ToolResult, error) {
	t.inputs = append(t.inputs, input)
	return t.result, nil
}

// corruptArgs is the misbehaving consumer the isolation contract must defend
// against: it rewrites the event's Args in place during delivery.
// {"n":1} -> {"n":9}: byte 5 is the digit.
func corruptArgs(ev AgentEvent) {
	if tc, ok := ev.(ToolCallEvent); ok && len(tc.Args) > 5 {
		tc.Args[5] = '9'
	}
}

func assistantToolInput(history []llm.Message) (json.RawMessage, bool) {
	for _, msg := range history {
		if msg.Role != llm.RoleAssistant {
			continue
		}
		for _, block := range msg.Content {
			if tb, ok := block.(*llm.ToolUseBlock); ok {
				return tb.Input, true
			}
		}
	}
	return nil, false
}

// TestRunStream_ToolCallEventArgsIsolated pins the streaming boundary
// contract: ToolCallEvent.Args is a private copy, so a consumer that mutates
// it in place cannot change what approval sees, what executes, or what the
// in-memory history retains for the next model round.
func TestRunStream_ToolCallEventArgsIsolated(t *testing.T) {
	capture := &recordingTool{
		info:   tool.ToolInfo{Name: "capture", Description: "captures args"},
		result: tool.NewTextResult("ok"),
	}
	reg := tool.NewRegistry()
	reg.MustRegister(capture)

	provider := NewMockStreamingProvider([][]llm.Chunk{
		{
			&llm.ToolCallStartChunk{Index: 0, ID: "c1", Name: "capture"},
			&llm.ToolCallArgsChunk{Index: 0, Delta: `{"n":1}`},
			&llm.DoneChunk{FinishReason: "tool_calls"},
		},
		{
			&llm.TextDeltaChunk{Text: "done"},
			&llm.DoneChunk{FinishReason: "stop"},
		},
	})

	agent := New(provider, reg, WithLogger(discardLogger()))
	seq, err := agent.RunStream(context.Background(), "run")
	if err != nil {
		t.Fatalf("RunStream returned error: %v", err)
	}

	var done *DoneEvent
	for ev, evErr := range seq {
		if evErr != nil {
			t.Fatalf("stream error: %v", evErr)
		}
		corruptArgs(ev)
		if d, ok := ev.(DoneEvent); ok {
			done = &d
		}
	}
	if done == nil {
		t.Fatal("stream ended without DoneEvent")
	}

	const original = `{"n":1}`
	if len(capture.inputs) != 1 {
		t.Fatalf("tool executions = %d, want 1", len(capture.inputs))
	}
	if got := string(capture.inputs[0]); got != original {
		t.Errorf("executed input = %s, want %s", got, original)
	}
	historyInput, ok := assistantToolInput(done.History)
	if !ok {
		t.Fatal("no ToolUseBlock found in DoneEvent.History")
	}
	if got := string(historyInput); got != original {
		t.Errorf("history tool_use input = %s, want %s", got, original)
	}
}

// TestRunStream_MaxIterEventArgsIsolated applies the same corruption during a
// maxIter terminal announcement: nothing executes there, but the history's
// assistant message must still keep the original bytes.
func TestRunStream_MaxIterEventArgsIsolated(t *testing.T) {
	capture := &recordingTool{
		info:   tool.ToolInfo{Name: "capture", Description: "captures args"},
		result: tool.NewTextResult("ok"),
	}
	reg := tool.NewRegistry()
	reg.MustRegister(capture)

	provider := NewMockStreamingProvider([][]llm.Chunk{
		{
			&llm.ToolCallStartChunk{Index: 0, ID: "c1", Name: "capture"},
			&llm.ToolCallArgsChunk{Index: 0, Delta: `{"n":1}`},
			&llm.DoneChunk{FinishReason: "tool_calls"},
		},
	})

	agent := New(provider, reg, WithMaxIter(1), WithLogger(discardLogger()))
	seq, err := agent.RunStream(context.Background(), "run")
	if err != nil {
		t.Fatalf("RunStream returned error: %v", err)
	}

	var done *DoneEvent
	for ev, evErr := range seq {
		if evErr != nil {
			t.Fatalf("stream error: %v", evErr)
		}
		corruptArgs(ev)
		if d, ok := ev.(DoneEvent); ok {
			done = &d
		}
	}
	if done == nil {
		t.Fatal("stream ended without DoneEvent")
	}
	if !done.Truncated {
		t.Error("DoneEvent.Truncated = false, want true")
	}
	if len(capture.inputs) != 0 {
		t.Errorf("tool executions = %d, want 0 (maxIter terminal round)", len(capture.inputs))
	}
	historyInput, ok := assistantToolInput(done.History)
	if !ok {
		t.Fatal("no ToolUseBlock found in DoneEvent.History")
	}
	if got := string(historyInput); got != `{"n":1}` {
		t.Errorf("history tool_use input = %s, want {\"n\":1}", got)
	}
}

// TestRunStream_ApprovalSeesOriginalArgs covers the approval seam: the
// callback must be shown the kernel's original bytes even while a consumer is
// corrupting the event copy during the announcement window.
func TestRunStream_ApprovalSeesOriginalArgs(t *testing.T) {
	capture := &recordingTool{
		info:   tool.ToolInfo{Name: "sensitive", Description: "needs approval", RequiresApproval: true},
		result: tool.NewTextResult("ok"),
	}
	reg := tool.NewRegistry()
	reg.MustRegister(capture)

	var approvedWith []json.RawMessage
	provider := NewMockStreamingProvider([][]llm.Chunk{
		{
			&llm.ToolCallStartChunk{Index: 0, ID: "c1", Name: "sensitive"},
			&llm.ToolCallArgsChunk{Index: 0, Delta: `{"n":1}`},
			&llm.DoneChunk{FinishReason: "tool_calls"},
		},
		{
			&llm.TextDeltaChunk{Text: "done"},
			&llm.DoneChunk{FinishReason: "stop"},
		},
	})

	agent := New(provider, reg,
		WithApprovalFn(func(_ tool.ToolInfo, input json.RawMessage) bool {
			approvedWith = append(approvedWith, input)
			return true
		}),
		WithLogger(discardLogger()),
	)
	seq, err := agent.RunStream(context.Background(), "run")
	if err != nil {
		t.Fatalf("RunStream returned error: %v", err)
	}
	for ev, evErr := range seq {
		if evErr != nil {
			t.Fatalf("stream error: %v", evErr)
		}
		corruptArgs(ev)
	}

	if len(approvedWith) != 1 {
		t.Fatalf("approvals = %d, want 1", len(approvedWith))
	}
	if got := string(approvedWith[0]); got != `{"n":1}` {
		t.Errorf("approval input = %s, want {\"n\":1}", got)
	}
	if got := string(capture.inputs[0]); got != `{"n":1}` {
		t.Errorf("executed input = %s, want {\"n\":1}", got)
	}
}

// TestRunStream_ZeroArgsEventPreservesNilness pins the empty-argument edge:
// the streaming assembler produces a non-nil empty RawMessage for a call with
// no args chunks, and bytes.Clone must keep that nilness on the event so the
// event copy matches the bytes the kernel executes with.
func TestRunStream_ZeroArgsEventPreservesNilness(t *testing.T) {
	capture := &recordingTool{
		info:   tool.ToolInfo{Name: "capture", Description: "captures args"},
		result: tool.NewTextResult("ok"),
	}
	reg := tool.NewRegistry()
	reg.MustRegister(capture)

	provider := NewMockStreamingProvider([][]llm.Chunk{
		{
			&llm.ToolCallStartChunk{Index: 0, ID: "c1", Name: "capture"},
			&llm.DoneChunk{FinishReason: "tool_calls"},
		},
		{
			&llm.TextDeltaChunk{Text: "done"},
			&llm.DoneChunk{FinishReason: "stop"},
		},
	})

	agent := New(provider, reg, WithLogger(discardLogger()))
	seq, err := agent.RunStream(context.Background(), "run")
	if err != nil {
		t.Fatalf("RunStream returned error: %v", err)
	}

	var eventArgs json.RawMessage
	for ev, evErr := range seq {
		if evErr != nil {
			t.Fatalf("stream error: %v", evErr)
		}
		if tc, ok := ev.(ToolCallEvent); ok {
			eventArgs = tc.Args
		}
	}

	if eventArgs == nil {
		t.Error("ToolCallEvent.Args = nil, want non-nil empty (must match executed bytes' nilness)")
	}
	if len(eventArgs) != 0 {
		t.Errorf("len(ToolCallEvent.Args) = %d, want 0", len(eventArgs))
	}
	if len(capture.inputs) != 1 {
		t.Fatalf("tool executions = %d, want 1", len(capture.inputs))
	}
	if !bytes.Equal(eventArgs, capture.inputs[0]) {
		t.Errorf("event args %q != executed input %q", eventArgs, capture.inputs[0])
	}
}
