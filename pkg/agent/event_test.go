package agent

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/dailz1/go-agent/pkg/llm"
	"github.com/dailz1/go-agent/pkg/tool"
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
