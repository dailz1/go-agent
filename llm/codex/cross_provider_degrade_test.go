package codex

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/dailz1/go-agent/llm"
)

func asCapability(t *testing.T, err error, code string) {
	t.Helper()
	var cerr *Error
	if !errors.As(err, &cerr) {
		t.Fatalf("err = %v, want *codex.Error", err)
	}
	if cerr.Kind != KindCapability || cerr.Code != code {
		t.Fatalf("err = %+v, want capability error %q", cerr, code)
	}
}

// TestConvertMessagesCrossProviderDegrade pins the codex rows of the
// cross-provider degradation table: the foreign CC-family reasoning text
// is skipped in assistant messages, the native reasoning item is replayed
// before its paired function_call, and a zero-argument tool call — legal
// history a GLM accumulator produces — fails closed with the existing
// tool_use capability error rather than being silently mangled.
func TestConvertMessagesCrossProviderDegrade(t *testing.T) {
	assistant := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		llm.ReasoningBlock{Type: "reasoning", Content: "foreign chain of thought"},
		llm.ReasoningItemBlock{ID: "rs_1", EncryptedContent: "enc"},
		llm.ToolUseBlock{ID: "call_1", Name: "f", Input: json.RawMessage(`{}`)},
	}}
	items, _, err := convertMessages([]llm.Message{llm.UserMessage("go"), assistant})
	if err != nil {
		t.Fatalf("convertMessages: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("items = %+v, want message + reasoning + function_call", items)
	}
	if items[1].Type != "reasoning" || items[1].ID != "rs_1" || items[1].EncryptedContent != "enc" {
		t.Errorf("reasoning item = %+v, want the native item replayed", items[1])
	}
	if items[2].Type != "function_call" || items[2].CallID != "call_1" {
		t.Errorf("function_call = %+v, want it ordered right after its reasoning item", items[2])
	}

	// A zero-argument tool call (GLM streaming emits no args chunks for
	// zero-parameter calls, so the accumulator records empty input) is
	// legal history that codex rejects at conversion, every round, until
	// compaction drops the group.
	zeroArg := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		llm.ToolUseBlock{ID: "call_zero", Name: "ping", Input: json.RawMessage("")},
	}}
	if _, _, err := convertMessages([]llm.Message{llm.UserMessage("go"), zeroArg}); err == nil {
		t.Fatal("zero-argument tool call accepted, want capability error")
	} else {
		asCapability(t, err, "tool_use")
	}

	// The missing-ID strictness on native reasoning items is existing
	// fail-loud behavior, not a foreign-block rejection.
	missingID := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		llm.ReasoningItemBlock{EncryptedContent: "enc"},
	}}
	if _, _, err := convertMessages([]llm.Message{llm.UserMessage("go"), missingID}); err == nil {
		t.Fatal("reasoning item without ID accepted, want capability error")
	} else {
		asCapability(t, err, "reasoning_item")
	}
}
