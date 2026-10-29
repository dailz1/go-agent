package glm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/llm"
)

// TestConvertAssistantStripsForeignReasoning pins the GLM row of the
// cross-provider degradation table: GLM accepts no reasoning in outbound
// messages, so both the CC-family reasoning text and the Responses-family
// reasoning item are stripped in every form; a zero-argument tool call is
// carried with empty arguments and no conversion error.
func TestConvertAssistantStripsForeignReasoning(t *testing.T) {
	messages := []llm.Message{{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		llm.ReasoningBlock{Type: "reasoning", Content: "chain of thought"},
		llm.ReasoningItemBlock{Type: "reasoning_item", ID: "rs_1", EncryptedContent: "enc"},
		&llm.ReasoningItemBlock{Type: "reasoning_item", ID: "rs_2", EncryptedContent: "enc"},
		llm.ToolUseBlock{Type: "tool_use", ID: "call", Name: "ping", Input: json.RawMessage("")},
		llm.TextBlock{Type: "text", Text: "answer"},
	}}}
	converted, err := convertMessages(messages)
	if err != nil {
		t.Fatalf("convertMessages: %v", err)
	}
	assistant := converted[0]
	if assistant.Content != "answer" {
		t.Errorf("content = %v, want the preserved text", assistant.Content)
	}
	if len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0].Function.Arguments != "" {
		t.Errorf("tool calls = %+v, want one zero-arg call with empty arguments", assistant.ToolCalls)
	}
	encoded, err := json.Marshal(assistant)
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{"chain of thought", "rs_1", "rs_2", "reasoning"} {
		if strings.Contains(string(encoded), leaked) {
			t.Errorf("stripped reasoning leaked into the outbound message (%q): %s", leaked, encoded)
		}
	}
}

// TestProviderThreadModelShadowsConstructorModel pins the options-first
// model resolution the thread config projection relies on: the folded
// thread model (an llm.WithModel option appended after the base options)
// mechanically shadows the constructor model on the wire.
func TestProviderThreadModelShadowsConstructorModel(t *testing.T) {
	bodyCh := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		bodyCh <- body
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	provider := NewProvider("key", "constructor-model", WithBaseURL(srv.URL))
	if _, _, err := provider.Chat(context.Background(), []llm.Message{llm.UserMessage("hi")}, nil, llm.WithModel("thread-model")); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	var body struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(<-bodyCh, &body); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	if body.Model != "thread-model" {
		t.Fatalf("wire model = %q, want thread-model shadowing constructor-model", body.Model)
	}
}
