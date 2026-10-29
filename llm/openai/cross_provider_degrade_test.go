package openai

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

// TestConvertAssistantCrossProviderDegrade pins the outbound degradation
// table for foreign history produced by other providers: the CC-family
// reasoning text is native and passes through as reasoning_content, the
// Responses-family reasoning item is skipped in both value and pointer
// form, and a zero-argument tool call (e.g. a GLM accumulator artifact)
// is carried as an empty-arguments tool call without a conversion error.
func TestConvertAssistantCrossProviderDegrade(t *testing.T) {
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
	if assistant.ReasoningContent == nil || *assistant.ReasoningContent != "chain of thought" {
		t.Errorf("reasoning_content = %v, want the native CC reasoning text passed through", assistant.ReasoningContent)
	}
	if assistant.Content != "answer" {
		t.Errorf("content = %v, want the preserved text", assistant.Content)
	}
	if len(assistant.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(assistant.ToolCalls))
	}
	if args := assistant.ToolCalls[0].Function.Arguments; args != "" {
		t.Errorf("zero-arg tool call arguments = %q, want empty string carried as-is", args)
	}
	encoded, err := json.Marshal(assistant)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "rs_1") || strings.Contains(string(encoded), "rs_2") {
		t.Errorf("reasoning item leaked into the outbound message: %s", encoded)
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
