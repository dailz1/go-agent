package glm

import (
	"testing"

	"github.com/dailz1/go-agent/llm"
)

// TestConvertMessagesSkipsPointerReasoningItem pins the both-forms
// convention: a *ReasoningItemBlock recorded by the Responses adapter (deep
// copies produce pointer forms) is silently stripped by the GLM adapter —
// never an error — while the other blocks are preserved.
func TestConvertMessagesSkipsPointerReasoningItem(t *testing.T) {
	messages := []llm.Message{
		llm.UserMessage("hi"),
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			&llm.ReasoningItemBlock{Type: "reasoning_item", ID: "rs_1", EncryptedContent: "enc"},
			llm.TextBlock{Type: "text", Text: "answer"},
		}},
	}
	converted, err := convertMessages(messages)
	if err != nil {
		t.Fatalf("convertMessages: %v", err)
	}
	assistant := converted[1]
	if assistant.Role != string(llm.RoleAssistant) {
		t.Errorf("role = %q", assistant.Role)
	}
	if assistant.Content != "answer" {
		t.Errorf("content = %v, want the preserved text", assistant.Content)
	}
}
