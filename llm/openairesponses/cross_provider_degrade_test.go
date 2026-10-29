package openairesponses

import (
	"encoding/json"
	"testing"

	"github.com/dailz1/go-agent/llm"
)

// TestAssistantItemsCrossProviderDegrade pins the Responses row of the
// cross-provider degradation table: the foreign CC-family reasoning text
// is dropped (it is not part of the item model), the native reasoning
// item is replayed with its ID and encrypted content immediately before
// the function_call it belongs to, and a zero-argument tool call is
// carried with empty arguments and no conversion error.
func TestAssistantItemsCrossProviderDegrade(t *testing.T) {
	m := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		llm.ReasoningBlock{Type: "reasoning", Content: "foreign chain of thought"},
		llm.ReasoningItemBlock{Type: "reasoning_item", ID: "rs_1", EncryptedContent: "enc"},
		llm.ToolUseBlock{Type: "tool_use", ID: "call_1", Name: "f", Input: json.RawMessage("")},
	}}
	items, err := assistantItems(m)
	if err != nil {
		t.Fatalf("assistantItems: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %+v, want reasoning + function_call only", items)
	}
	if items[0].Type != "reasoning" || items[0].ID != "rs_1" || items[0].EncryptedContent != "enc" {
		t.Errorf("first item = %+v, want the native reasoning item replayed", items[0])
	}
	if items[1].Type != "function_call" || items[1].CallID != "call_1" || items[1].Arguments != "" {
		t.Errorf("second item = %+v, want the paired function_call with empty arguments", items[1])
	}
}

// TestBuildRequestThreadModelShadowsDefault pins the options-first model
// resolution the thread config projection relies on: a folded thread
// model mechanically shadows the provider's default model, and without an
// option the default applies.
func TestBuildRequestThreadModelShadowsDefault(t *testing.T) {
	items, instructions, err := convertMessages([]llm.Message{llm.UserMessage("hi")})
	if err != nil {
		t.Fatalf("convertMessages: %v", err)
	}
	req, err := buildRequest(llm.ApplyOptions([]llm.Option{llm.WithModel("thread-model")}), "default-model", items, instructions, nil, false)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	if req.Model != "thread-model" {
		t.Errorf("model = %q, want thread-model shadowing default-model", req.Model)
	}
	req, err = buildRequest(llm.ApplyOptions(nil), "default-model", items, instructions, nil, false)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	if req.Model != "default-model" {
		t.Errorf("model = %q, want default-model without an option", req.Model)
	}
}
