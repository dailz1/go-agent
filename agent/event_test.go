package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

// Compile-time sealed interface checks.
var _ AgentEvent = TextDeltaEvent{}
var _ AgentEvent = ToolCallEvent{}
var _ AgentEvent = ToolResultEvent{}
var _ AgentEvent = DoneEvent{}
var _ AgentEvent = ThinkingDeltaEvent{}
var _ AgentEvent = RetryEvent{}
var _ AgentEvent = CompactionEvent{}

func TestAgentEventSealedInterface(t *testing.T) {
	events := []AgentEvent{
		TextDeltaEvent{Text: "hello"},
		ThinkingDeltaEvent{Text: "hmm"},
		ToolCallEvent{ID: "call_1", Name: "search", Args: json.RawMessage(`{"q":"test"}`)},
		ToolResultEvent{ID: "call_1", Name: "search", Result: &tool.ToolResult{Content: "found"}},
		RetryEvent{RetryInfo: RetryInfo{Attempt: 2, MaxAttempts: 4, Delay: 500 * time.Millisecond, Reason: "rate limited (429)"}},
		DoneEvent{Message: llm.Message{}, History: []llm.Message{}, ToolCalls: 0, Truncated: false},
		CompactionEvent{},
	}

	expected := []string{"text_delta", "thinking_delta", "tool_call", "tool_result", "retry", "done", "compaction"}
	for i, ev := range events {
		if ev.eventType() != expected[i] {
			t.Errorf("event[%d].eventType() = %q, want %q", i, ev.eventType(), expected[i])
		}
	}
}

// TestEphemeralDoneEventThreadIDEmpty pins the ephemeral-run guarantee on
// the event surface: with a store configured but no WithThread, the
// terminal DoneEvent carries an empty ThreadID.
func TestEphemeralDoneEventThreadIDEmpty(t *testing.T) {
	a := persistTestAgent(t, store.NewMemory(), MsgResponse(llm.AssistantMessage("x")))
	seq, err := a.RunStream(context.Background(), "hi")
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	done := 0
	for ev, err := range seq {
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		if d, ok := ev.(DoneEvent); ok {
			done++
			if d.ThreadID != "" {
				t.Errorf("DoneEvent.ThreadID = %q, want \"\" for an ephemeral run", d.ThreadID)
			}
		}
	}
	if done != 1 {
		t.Fatalf("done events = %d, want 1", done)
	}
}
