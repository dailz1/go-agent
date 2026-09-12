package openairesponses

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/pkg/llm"
	"github.com/dailz1/go-agent/pkg/tool"
)

// TestChatAssemblesOutputItems pins the non-streaming round trip: request
// carries store:false + encrypted-reasoning include + instructions; output
// items assemble into reasoning/text/tool-use blocks in order.
func TestChatAssemblesOutputItems(t *testing.T) {
	var gotBody createRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     "resp_1",
			"status": "completed",
			"output": []map[string]any{
				{"type": "reasoning", "id": "rs_1", "encrypted_content": "enc-abc",
					"summary": []map[string]any{{"type": "summary_text", "text": "thinking"}}},
				{"type": "message", "role": "assistant",
					"content": []map[string]any{{"type": "output_text", "text": "hello"}}},
				{"type": "function_call", "call_id": "call_1", "name": "echo",
					"arguments": `{"x":1}`},
			},
			"usage": map[string]any{"input_tokens": 10, "output_tokens": 5, "total_tokens": 15,
				"output_tokens_details": map[string]any{"reasoning_tokens": 3}},
		})
	}))
	t.Cleanup(srv.Close)
	p := NewProvider("k", "gpt-test", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))

	messages := []llm.Message{
		llm.SystemMessage("be terse"),
		llm.UserMessage("hi"),
	}
	msg, usage, err := p.Chat(context.Background(), messages, nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	if gotBody.Store {
		t.Error("request store = true, want false")
	}
	if len(gotBody.Include) != 1 || gotBody.Include[0] != "reasoning.encrypted_content" {
		t.Errorf("include = %v, want [reasoning.encrypted_content]", gotBody.Include)
	}
	if gotBody.Instructions != "be terse" {
		t.Errorf("instructions = %q", gotBody.Instructions)
	}
	if gotBody.Model != "gpt-test" {
		t.Errorf("model = %q", gotBody.Model)
	}

	if msg.Role != llm.RoleAssistant || len(msg.Content) != 3 {
		t.Fatalf("message = %+v, want 3 blocks", msg)
	}
	ri, ok := msg.Content[0].(llm.ReasoningItemBlock)
	if !ok || ri.ID != "rs_1" || ri.EncryptedContent != "enc-abc" || len(ri.Summary) != 1 {
		t.Errorf("content[0] = %+v, want reasoning item rs_1", msg.Content[0])
	}
	tb, ok := msg.Content[1].(llm.TextBlock)
	if !ok || tb.Text != "hello" {
		t.Errorf("content[1] = %+v, want text hello", msg.Content[1])
	}
	ub, ok := msg.Content[2].(llm.ToolUseBlock)
	if !ok || ub.ID != "call_1" || ub.Name != "echo" {
		t.Errorf("content[2] = %+v, want tool_use call_1", msg.Content[2])
	}
	if usage.InputTokens != 10 || usage.OutputTokens != 5 || usage.ReasoningTokens != 3 {
		t.Errorf("usage = %+v", usage)
	}
}

// TestChatStreamTranslatesEvents pins the event→chunk translation: text and
// reasoning summary deltas, function-call start/args pairing by output index,
// the reasoning item delivered at its own position, usage on the terminal
// chunk, and unknown events ignored without error.
func TestChatStreamTranslatesEvents(t *testing.T) {
	events := []string{
		`{"type":"response.created","response":{"id":"resp_1"}}`,
		`{"type":"response.reasoning_summary_text.delta","output_index":0,"delta":"think"}`,
		`{"type":"response.output_text.delta","output_index":1,"delta":"Hel"}`,
		`{"type":"response.output_text.delta","output_index":1,"delta":"lo"}`,
		`{"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","call_id":"call_9","name":"echo"}}`,
		`{"type":"response.function_call_arguments.delta","output_index":2,"delta":"{\"x\":"}`,
		`{"type":"response.function_call_arguments.delta","output_index":2,"delta":"1}"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1","encrypted_content":"enc"}}`,
		`{"type":"some.future.event","data":"ignored"}`,
		`{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"type":"reasoning","id":"rs_1","encrypted_content":"enc"},{"type":"message","role":"assistant","content":[]},{"type":"function_call","call_id":"call_9","name":"echo","arguments":"{\"x\":1}"}],"usage":{"input_tokens":7,"output_tokens":4,"total_tokens":11}}}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range events {
			_, _ = w.Write([]byte("data: " + ev + "\n\n"))
		}
	}))
	t.Cleanup(srv.Close)
	p := NewProvider("k", "gpt-test", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))

	seq, err := p.ChatStream(context.Background(), []llm.Message{llm.UserMessage("hi")}, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}

	var chunks []llm.Chunk
	for c, err := range seq {
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		chunks = append(chunks, c)
	}

	var texts, reasonings string
	var start *llm.ToolCallStartChunk
	var args string
	var done *llm.DoneChunk
	var itemChunks []llm.ReasoningItemChunk
	var textIndex int64 = -1
	for _, c := range chunks {
		switch e := c.(type) {
		case llm.TextDeltaChunk:
			texts += e.Text
			textIndex = e.OutputIndex
		case llm.ReasoningDeltaChunk:
			reasonings += e.Text
		case llm.ToolCallStartChunk:
			cp := e
			start = &cp
		case llm.ToolCallArgsChunk:
			args += e.Delta
		case llm.ReasoningItemChunk:
			itemChunks = append(itemChunks, e)
		case llm.DoneChunk:
			cp := e
			done = &cp
		}
	}
	if texts != "Hello" {
		t.Errorf("text = %q, want Hello", texts)
	}
	if textIndex != 1 {
		t.Errorf("text output_index = %d, want 1", textIndex)
	}
	if reasonings != "think" {
		t.Errorf("reasoning = %q, want think", reasonings)
	}
	if start == nil || start.ID != "call_9" || start.Index != 2 || start.Name != "echo" {
		t.Errorf("start = %+v, want call_9/echo at index 2", start)
	}
	if args != `{"x":1}` {
		t.Errorf("args = %q", args)
	}
	if done == nil {
		t.Fatal("no DoneChunk")
	}
	if done.Usage == nil || done.Usage.InputTokens != 7 || done.Usage.OutputTokens != 4 {
		t.Errorf("done usage = %+v", done.Usage)
	}
	// The encrypted reasoning item is delivered exactly once: the fixture
	// sends it BOTH via output_item.done and inside the completed payload —
	// the completed handler must dedupe by item ID, not duplicate it.
	if len(itemChunks) != 1 {
		t.Fatalf("reasoning item chunks = %d, want exactly 1 (deduplicated)", len(itemChunks))
	}
	if itemChunks[0].Item.ID != "rs_1" || itemChunks[0].Item.EncryptedContent != "enc" || itemChunks[0].OutputIndex != 0 {
		t.Errorf("reasoning item chunk = %+v, want rs_1 with encrypted content at index 0", itemChunks[0])
	}
}

// TestChatStreamFailedResponse pins that failure and error events surface as
// stream errors.
func TestChatStreamFailedResponse(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event string
	}{
		{"failed", `{"type":"response.failed","response":{"error":{"message":"boom"}}}`},
		{"error", `{"type":"error","message":"boom"}`},
		{"incomplete", `{"type":"response.incomplete","response":{"incomplete_details":{"reason":"max_output_tokens"}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte("data: " + tc.event + "\n\n"))
			}))
			t.Cleanup(srv.Close)
			p := NewProvider("k", "m", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
			seq, err := p.ChatStream(context.Background(), []llm.Message{llm.UserMessage("hi")}, nil)
			if err != nil {
				t.Fatalf("ChatStream: %v", err)
			}
			for _, err := range seq {
				if err == nil {
					t.Error("expected an error yield, got chunks without error")
					break
				}
				return
			}
			t.Error("stream ended without an error yield")
		})
	}
}

// TestConvertMessagesItemSequence pins the message→items mapping: system →
// instructions, user items, assistant fan-out (reasoning before its
// function_call), and tool results as function_call_output.
func TestConvertMessagesItemSequence(t *testing.T) {
	assistant := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		llm.ReasoningItemBlock{Type: "reasoning_item", ID: "rs_1", EncryptedContent: "enc",
			Summary: []string{"thought"}},
		llm.TextBlock{Type: "text", Text: "let me check"},
		llm.ToolUseBlock{Type: "tool_use", ID: "call_1", Name: "echo", Input: json.RawMessage(`{}`)},
	}}
	messages := []llm.Message{
		llm.SystemMessage("sys prompt"),
		llm.UserMessage("go"),
		assistant,
		llm.ToolResultMessage("call_1", tool.NewTextResult("echoed")),
	}

	items, instructions, err := convertMessages(messages)
	if err != nil {
		t.Fatalf("convertMessages: %v", err)
	}
	if instructions != "sys prompt" {
		t.Errorf("instructions = %q", instructions)
	}
	wantTypes := []string{"message", "reasoning", "message", "function_call", "function_call_output"}
	if len(items) != len(wantTypes) {
		t.Fatalf("items = %d, want %d", len(items), len(wantTypes))
	}
	for i, wt := range wantTypes {
		if items[i].Type != wt {
			t.Errorf("items[%d].Type = %q, want %q", i, items[i].Type, wt)
		}
	}
	// The reasoning item precedes its function_call and carries the
	// encrypted content verbatim.
	if items[1].ID != "rs_1" || items[1].EncryptedContent != "enc" {
		t.Errorf("reasoning item = %+v", items[1])
	}
	if items[3].CallID != "call_1" || items[3].Arguments != `{}` {
		t.Errorf("function_call = %+v", items[3])
	}
	if items[4].CallID != "call_1" || items[4].Output != "echoed" {
		t.Errorf("function_call_output = %+v", items[4])
	}
}

// TestConvertToolDefs pins strict:false and schema passthrough.
func TestConvertToolDefs(t *testing.T) {
	defs, err := convertToolDefs([]tool.ToolInfo{{
		Name:        "echo",
		Description: "echoes",
		Parameters: tool.ParameterSchema{Type: "object", Properties: map[string]tool.Property{
			"x": tool.Param("number", "x value"),
		}, Required: []string{"x"}},
	}})
	if err != nil {
		t.Fatalf("convertToolDefs: %v", err)
	}
	if len(defs) != 1 {
		t.Fatalf("defs = %d, want 1", len(defs))
	}
	if defs[0].Type != "function" || defs[0].Name != "echo" || defs[0].Strict {
		t.Errorf("def = %+v, want function/echo/strict=false", defs[0])
	}
	if len(defs[0].Parameters) == 0 || !strings.Contains(string(defs[0].Parameters), `"type":"object"`) {
		t.Errorf("parameters = %s, want the object schema", defs[0].Parameters)
	}
}
