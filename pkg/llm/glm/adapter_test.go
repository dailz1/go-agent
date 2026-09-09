package glm

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dailz1/go-agent/pkg/llm"
	"github.com/dailz1/go-agent/pkg/tool"
)

// --- NewProvider / Name ---

func TestNewProvider_Defaults(t *testing.T) {
	p := NewProvider("test-key", "glm-4-flash")
	if p.baseURL != defaultBaseURL {
		t.Errorf("baseURL = %q, want %q", p.baseURL, defaultBaseURL)
	}
	if p.apiKey != "test-key" {
		t.Errorf("apiKey = %q, want %q", p.apiKey, "test-key")
	}
	if p.model != "glm-4-flash" {
		t.Errorf("model = %q, want %q", p.model, "glm-4-flash")
	}
	if p.httpClient == nil {
		t.Error("httpClient should not be nil")
	}
	if p.logger == nil {
		t.Error("logger should not be nil")
	}
}

func TestNewProvider_WithOptions(t *testing.T) {
	customURL := "http://localhost:8080"
	p := NewProvider("key", "model",
		WithBaseURL(customURL),
	)
	if p.baseURL != customURL {
		t.Errorf("baseURL = %q, want %q", p.baseURL, customURL)
	}
}

func TestNewProvider_AuthMode_Bearer(t *testing.T) {
	p := NewProvider("simple-key", "model")
	if p.authMode != "bearer" {
		t.Errorf("authMode = %q, want %q", p.authMode, "bearer")
	}
}

func TestNewProvider_AuthMode_JWT(t *testing.T) {
	p := NewProvider("id.secret", "model")
	if p.authMode != "jwt" {
		t.Errorf("authMode = %q, want %q", p.authMode, "jwt")
	}
}

func TestProvider_Name(t *testing.T) {
	p := NewProvider("key", "model")
	if got := p.Name(); got != "glm" {
		t.Errorf("Name() = %q, want %q", got, "glm")
	}
}

// --- convertMessages ---

func TestConvertMessages_SystemRole(t *testing.T) {
	msgs := []llm.Message{
		llm.SystemMessage("you are helpful"),
	}
	result, err := convertMessages(msgs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 message, got %d", len(result))
	}
	cm := result[0]
	if cm.Role != "system" {
		t.Errorf("role = %q, want %q", cm.Role, "system")
	}
	s, ok := cm.Content.(string)
	if !ok {
		t.Fatalf("expected string content, got %T", cm.Content)
	}
	if s != "you are helpful" {
		t.Errorf("content = %q, want %q", s, "you are helpful")
	}
}

func TestConvertMessages_UserText(t *testing.T) {
	msgs := []llm.Message{
		llm.UserMessage("hello"),
	}
	result, err := convertMessages(msgs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s, ok := result[0].Content.(string)
	if !ok {
		t.Fatalf("expected string content, got %T", result[0].Content)
	}
	if s != "hello" {
		t.Errorf("content = %q, want %q", s, "hello")
	}
}

func TestConvertMessages_UserWithImage(t *testing.T) {
	msgs := []llm.Message{
		{
			Role: llm.RoleUser,
			Content: []llm.ContentBlock{
				llm.TextBlock{Type: "text", Text: "describe this"},
				llm.ImageBlock{Type: "image", URL: "https://example.com/img.png"},
			},
		},
	}
	result, err := convertMessages(msgs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cm := result[0]
	if cm.Role != "user" {
		t.Errorf("role = %q, want %q", cm.Role, "user")
	}
	parts, ok := cm.Content.([]contentPart)
	if !ok {
		t.Fatalf("expected []contentPart, got %T", cm.Content)
	}
	if len(parts) != 2 {
		t.Fatalf("expected 2 parts, got %d", len(parts))
	}
	if parts[0].Type != "text" || parts[0].Text != "describe this" {
		t.Errorf("part[0] = %+v, want text block", parts[0])
	}
	if parts[1].Type != "image_url" || parts[1].ImageURL == nil || parts[1].ImageURL.URL != "https://example.com/img.png" {
		t.Errorf("part[1] = %+v, want image_url block", parts[1])
	}
}

func TestConvertMessages_UserWithInlineImage(t *testing.T) {
	msgs := []llm.Message{
		{
			Role: llm.RoleUser,
			Content: []llm.ContentBlock{
				llm.ImageBlock{Type: "image", MIMEType: "image/png", Data: []byte("fake-png-data")},
			},
		},
	}
	result, err := convertMessages(msgs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	parts, ok := result[0].Content.([]contentPart)
	if !ok {
		t.Fatalf("expected []contentPart, got %T", result[0].Content)
	}
	if len(parts) != 1 {
		t.Fatalf("expected 1 part, got %d", len(parts))
	}
	wantPrefix := "data:image/png;base64,"
	if parts[0].ImageURL == nil || parts[0].ImageURL.URL[:len(wantPrefix)] != wantPrefix {
		t.Errorf("image URL = %q, want prefix %q", parts[0].ImageURL.URL, wantPrefix)
	}
}

func TestConvertMessages_AssistantWithToolCalls(t *testing.T) {
	msgs := []llm.Message{
		llm.AssistantToolCallMessage(
			llm.ToolUseBlock{
				Type:  "tool_use",
				ID:    "call_123",
				Name:  "get_weather",
				Input: json.RawMessage(`{"city":"Beijing"}`),
			},
		),
	}
	result, err := convertMessages(msgs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cm := result[0]
	if cm.Role != "assistant" {
		t.Errorf("role = %q, want %q", cm.Role, "assistant")
	}
	if cm.Content != "" {
		t.Errorf("content = %q, want empty string", cm.Content)
	}
	if len(cm.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(cm.ToolCalls))
	}
	tc := cm.ToolCalls[0]
	if tc.ID != "call_123" {
		t.Errorf("tool call ID = %q, want %q", tc.ID, "call_123")
	}
	if tc.Function.Name != "get_weather" {
		t.Errorf("function name = %q, want %q", tc.Function.Name, "get_weather")
	}
	if tc.Function.Arguments != `{"city":"Beijing"}` {
		t.Errorf("function arguments = %q, want %q", tc.Function.Arguments, `{"city":"Beijing"}`)
	}
}

func TestConvertMessages_ToolResult(t *testing.T) {
	msgs := []llm.Message{
		llm.ToolResultMessage("call_123", &tool.ToolResult{Content: "sunny, 22°C"}),
	}
	result, err := convertMessages(msgs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cm := result[0]
	if cm.Role != "tool" {
		t.Errorf("role = %q, want %q", cm.Role, "tool")
	}
	if cm.ToolCallID != "call_123" {
		t.Errorf("tool_call_id = %q, want %q", cm.ToolCallID, "call_123")
	}
	s, ok := cm.Content.(string)
	if !ok {
		t.Fatalf("expected string content, got %T", cm.Content)
	}
	if s != "sunny, 22°C" {
		t.Errorf("content = %q, want %q", s, "sunny, 22°C")
	}
}

func TestConvertMessages_ToolResultMissing(t *testing.T) {
	msgs := []llm.Message{
		{
			Role:    llm.RoleTool,
			Content: []llm.ContentBlock{llm.TextBlock{Type: "text", Text: "oops"}},
		},
	}
	_, err := convertMessages(msgs)
	if err == nil {
		t.Fatal("expected error for tool message without ToolResultBlock")
	}
}

// --- convertToolDefs ---

func TestConvertToolDefs_Single(t *testing.T) {
	tools := []tool.ToolInfo{
		{
			Name:        "get_weather",
			Description: "Get weather for a city",
			Parameters: tool.ParameterSchema{
				Type: "object",
				Properties: map[string]tool.Property{
					"city": {Type: "string", Description: "City name"},
				},
				Required: []string{"city"},
			},
		},
	}
	defs, err := convertToolDefs(tools)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(defs) != 1 {
		t.Fatalf("expected 1 def, got %d", len(defs))
	}
	d := defs[0]
	if d.Type != "function" {
		t.Errorf("type = %q, want %q", d.Type, "function")
	}
	if d.Function.Name != "get_weather" {
		t.Errorf("name = %q, want %q", d.Function.Name, "get_weather")
	}
	if d.Function.Description != "Get weather for a city" {
		t.Errorf("description = %q, want %q", d.Function.Description, "Get weather for a city")
	}
	var params map[string]any
	if err := json.Unmarshal(d.Function.Parameters, &params); err != nil {
		t.Fatalf("parameters not valid JSON: %v", err)
	}
}

func TestConvertToolDefs_Multiple(t *testing.T) {
	tools := []tool.ToolInfo{
		{
			Name:       "tool_a",
			Parameters: tool.ParameterSchema{Type: "object"},
		},
		{
			Name:       "tool_b",
			Parameters: tool.ParameterSchema{Type: "object"},
		},
	}
	defs, err := convertToolDefs(tools)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(defs) != 2 {
		t.Fatalf("expected 2 defs, got %d", len(defs))
	}
	if defs[0].Function.Name != "tool_a" {
		t.Errorf("defs[0].Name = %q, want %q", defs[0].Function.Name, "tool_a")
	}
	if defs[1].Function.Name != "tool_b" {
		t.Errorf("defs[1].Name = %q, want %q", defs[1].Function.Name, "tool_b")
	}
}

// --- convertResponseMessage ---

func TestConvertResponseMessage_TextContent(t *testing.T) {
	content := "Hello, world!"
	msg, err := convertResponseMessage(respMessage{
		Role:    "assistant",
		Content: &content,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if msg.Role != llm.RoleAssistant {
		t.Errorf("role = %q, want %q", msg.Role, llm.RoleAssistant)
	}
	if len(msg.Content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(msg.Content))
	}
	tb, ok := msg.Content[0].(llm.TextBlock)
	if !ok {
		t.Fatalf("expected TextBlock, got %T", msg.Content[0])
	}
	if tb.Text != "Hello, world!" {
		t.Errorf("text = %q, want %q", tb.Text, "Hello, world!")
	}
}

func TestConvertResponseMessage_ToolCalls(t *testing.T) {
	content := ""
	msg, err := convertResponseMessage(respMessage{
		Role:    "assistant",
		Content: &content,
		ToolCalls: []toolCall{
			{
				ID:   "call_abc",
				Type: "function",
				Function: functionCall{
					Name:      "search",
					Arguments: `{"q":"golang"}`,
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(msg.Content) != 1 {
		t.Fatalf("expected 1 content block (tool_use only), got %d", len(msg.Content))
	}
	tu, ok := msg.Content[0].(llm.ToolUseBlock)
	if !ok {
		t.Fatalf("expected ToolUseBlock, got %T", msg.Content[0])
	}
	if tu.ID != "call_abc" {
		t.Errorf("id = %q, want %q", tu.ID, "call_abc")
	}
	if tu.Name != "search" {
		t.Errorf("name = %q, want %q", tu.Name, "search")
	}
	if string(tu.Input) != `{"q":"golang"}` {
		t.Errorf("input = %q, want %q", string(tu.Input), `{"q":"golang"}`)
	}
}

func TestConvertResponseMessage_ToolCallsWithContent(t *testing.T) {
	content := "Let me search for that."
	msg, err := convertResponseMessage(respMessage{
		Role:    "assistant",
		Content: &content,
		ToolCalls: []toolCall{
			{
				ID:   "call_1",
				Type: "function",
				Function: functionCall{
					Name:      "search",
					Arguments: `{"q":"test"}`,
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(msg.Content) != 2 {
		t.Fatalf("expected 2 content blocks, got %d", len(msg.Content))
	}
	tb, ok := msg.Content[0].(llm.TextBlock)
	if !ok {
		t.Fatalf("expected TextBlock at [0], got %T", msg.Content[0])
	}
	if tb.Text != "Let me search for that." {
		t.Errorf("text = %q, want %q", tb.Text, "Let me search for that.")
	}
}

func TestConvertResponseMessage_EmptyMessage(t *testing.T) {
	_, err := convertResponseMessage(respMessage{
		Role: "assistant",
	})
	if err == nil {
		t.Fatal("expected error for empty response message")
	}
}

func TestConvertResponseMessage_NilContentWithToolCalls(t *testing.T) {
	msg, err := convertResponseMessage(respMessage{
		Role:    "assistant",
		Content: nil,
		ToolCalls: []toolCall{
			{
				ID:   "call_1",
				Type: "function",
				Function: functionCall{
					Name:      "fn",
					Arguments: `{}`,
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(msg.Content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(msg.Content))
	}
}

// --- parseStreamPayload ---

func mustParseStreamPayload(t *testing.T, payload string) []llm.Chunk {
	t.Helper()
	chunks, err := parseStreamPayload(payload)
	if err != nil {
		t.Fatalf("parseStreamPayload() error = %v", err)
	}
	return chunks
}

func TestParseStreamPayload_ContentDelta(t *testing.T) {
	payload := `{"choices":[{"delta":{"content":"hello"},"finish_reason":null}]}`
	chunks := mustParseStreamPayload(t, payload)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	td, ok := chunks[0].(llm.TextDeltaChunk)
	if !ok {
		t.Fatalf("expected TextDeltaChunk, got %T", chunks[0])
	}
	if td.Text != "hello" {
		t.Errorf("text = %q, want %q", td.Text, "hello")
	}
}

func TestParseStreamPayload_ReasoningDelta(t *testing.T) {
	payload := `{"choices":[{"delta":{"reasoning_content":"thinking..."}}]}`
	chunks := mustParseStreamPayload(t, payload)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	rd, ok := chunks[0].(llm.ReasoningDeltaChunk)
	if !ok {
		t.Fatalf("expected ReasoningDeltaChunk, got %T", chunks[0])
	}
	if rd.Text != "thinking..." {
		t.Errorf("text = %q, want %q", rd.Text, "thinking...")
	}
}

func TestParseStreamPayload_ToolCallStart(t *testing.T) {
	payload := `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"search"}}]}}]}`
	chunks := mustParseStreamPayload(t, payload)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	tcs, ok := chunks[0].(llm.ToolCallStartChunk)
	if !ok {
		t.Fatalf("expected ToolCallStartChunk, got %T", chunks[0])
	}
	if tcs.Index != 0 {
		t.Errorf("index = %d, want 0", tcs.Index)
	}
	if tcs.ID != "call_1" {
		t.Errorf("id = %q, want %q", tcs.ID, "call_1")
	}
	if tcs.Name != "search" {
		t.Errorf("name = %q, want %q", tcs.Name, "search")
	}
}

func TestParseStreamPayload_ToolCallArgs(t *testing.T) {
	payload := `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"search","arguments":"{\"q\":"}}]}}]}`
	chunks := mustParseStreamPayload(t, payload)
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(chunks))
	}
	_, okStart := chunks[0].(llm.ToolCallStartChunk)
	if !okStart {
		t.Fatalf("expected ToolCallStartChunk at [0], got %T", chunks[0])
	}
	tca, okArgs := chunks[1].(llm.ToolCallArgsChunk)
	if !okArgs {
		t.Fatalf("expected ToolCallArgsChunk at [1], got %T", chunks[1])
	}
	if tca.Delta != `{"q":` {
		t.Errorf("delta = %q, want %q", tca.Delta, `{"q":`)
	}
}

func TestParseStreamPayload_ToolCallArgsOnly(t *testing.T) {
	payload := `{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"go\"}"}}]}}]}`
	chunks := mustParseStreamPayload(t, payload)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	tca, ok := chunks[0].(llm.ToolCallArgsChunk)
	if !ok {
		t.Fatalf("expected ToolCallArgsChunk, got %T", chunks[0])
	}
	if tca.Delta != `"go"}` {
		t.Errorf("delta = %q, want %q", tca.Delta, `"go"}`)
	}
}

func TestParseStreamPayload_FinishReason(t *testing.T) {
	payload := `{"choices":[{"delta":{},"finish_reason":"stop"}]}`
	chunks := mustParseStreamPayload(t, payload)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	dc, ok := chunks[0].(llm.DoneChunk)
	if !ok {
		t.Fatalf("expected DoneChunk, got %T", chunks[0])
	}
	if dc.FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want %q", dc.FinishReason, "stop")
	}
	if dc.Usage != nil {
		t.Errorf("expected nil Usage when no usage in payload, got %+v", dc.Usage)
	}
}

func TestParseStreamPayload_WithUsage(t *testing.T) {
	payload := `{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":8,"completion_tokens":262,"total_tokens":270}}`
	chunks := mustParseStreamPayload(t, payload)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	dc, ok := chunks[0].(llm.DoneChunk)
	if !ok {
		t.Fatalf("expected DoneChunk, got %T", chunks[0])
	}
	if dc.FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want %q", dc.FinishReason, "stop")
	}
	if dc.Usage == nil {
		t.Fatal("expected non-nil Usage in DoneChunk")
	}
	if dc.Usage.InputTokens != 8 {
		t.Errorf("InputTokens = %d, want 8", dc.Usage.InputTokens)
	}
	if dc.Usage.OutputTokens != 262 {
		t.Errorf("OutputTokens = %d, want 262", dc.Usage.OutputTokens)
	}
}

func TestParseStreamPayload_UsageOnlyFrame(t *testing.T) {
	payload := `{"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`
	chunks := mustParseStreamPayload(t, payload)
	if len(chunks) != 1 {
		t.Fatalf("expected exactly 1 chunk, got %d", len(chunks))
	}
	done, ok := chunks[0].(llm.DoneChunk)
	if !ok {
		t.Fatalf("expected DoneChunk, got %T", chunks[0])
	}
	if done.FinishReason != "" {
		t.Errorf("FinishReason = %q, want empty", done.FinishReason)
	}
	if done.Usage == nil {
		t.Fatal("expected non-nil Usage in DoneChunk")
	}
	if done.Usage.InputTokens != 7 || done.Usage.OutputTokens != 3 {
		t.Errorf("Usage = %+v, want input=7 output=3", done.Usage)
	}
}

func TestParseStreamPayload_InvalidJSON(t *testing.T) {
	chunks, err := parseStreamPayload("not json")
	if err == nil {
		t.Fatal("parseStreamPayload() error = nil, want malformed payload error")
	}
	if chunks != nil {
		t.Errorf("parseStreamPayload() chunks = %v, want nil", chunks)
	}
}

func TestParseStreamPayload_EmptyPayload(t *testing.T) {
	chunks, err := parseStreamPayload(" \n\t")
	if err != nil {
		t.Fatalf("parseStreamPayload() error = %v", err)
	}
	if chunks != nil {
		t.Errorf("parseStreamPayload() chunks = %v, want nil", chunks)
	}
}

func TestParseStreamPayload_EmptyContent(t *testing.T) {
	payload := `{"choices":[{"delta":{"content":""}}]}`
	chunks := mustParseStreamPayload(t, payload)
	if len(chunks) != 0 {
		t.Errorf("expected 0 chunks for empty content, got %d", len(chunks))
	}
}

func TestParseStreamPayload_MultipleChunks(t *testing.T) {
	payload := `{"choices":[{"delta":{"content":"hi","reasoning_content":"think"},"finish_reason":"stop"}]}`
	chunks := mustParseStreamPayload(t, payload)
	if len(chunks) != 3 {
		t.Fatalf("expected 3 chunks, got %d", len(chunks))
	}
	if _, ok := chunks[0].(llm.ReasoningDeltaChunk); !ok {
		t.Errorf("chunk[0] = %T, want ReasoningDeltaChunk", chunks[0])
	}
	if _, ok := chunks[1].(llm.TextDeltaChunk); !ok {
		t.Errorf("chunk[1] = %T, want TextDeltaChunk", chunks[1])
	}
	if _, ok := chunks[2].(llm.DoneChunk); !ok {
		t.Errorf("chunk[2] = %T, want DoneChunk", chunks[2])
	}
}

// --- firstNonEmpty ---

func TestFirstNonEmpty(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"first non-empty", []string{"", "hello", "world"}, "hello"},
		{"first is non-empty", []string{"yes", "no"}, "yes"},
		{"all empty", []string{"", "", ""}, ""},
		{"no args", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := firstNonEmpty(tt.args...)
			if got != tt.want {
				t.Errorf("firstNonEmpty(%v) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}

// --- ptrToString ---

func TestPtrToString(t *testing.T) {
	s := "hello"
	if got := ptrToString(&s); got != "hello" {
		t.Errorf("ptrToString(&hello) = %q, want %q", got, "hello")
	}
	if got := ptrToString(nil); got != "" {
		t.Errorf("ptrToString(nil) = %q, want empty", got)
	}
}

// --- encodeBase64 ---

func TestEncodeBase64(t *testing.T) {
	data := []byte("hello")
	want := "aGVsbG8="
	if got := encodeBase64(data); got != want {
		t.Errorf("encodeBase64(%q) = %q, want %q", data, got, want)
	}
}

// --- apiHeaders ---

func TestApiHeaders(t *testing.T) {
	p := NewProvider("my-secret-key", "model")
	headers, err := p.apiHeaders()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if headers["Authorization"] != "Bearer my-secret-key" {
		t.Errorf("Authorization = %q, want %q", headers["Authorization"], "Bearer my-secret-key")
	}
}

func TestApiHeaders_BearerAuth(t *testing.T) {
	p := NewProvider("plain-bearer-token", "model")
	if p.authMode != "bearer" {
		t.Fatalf("expected bearer auth mode, got %q", p.authMode)
	}
	headers, err := p.apiHeaders()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if headers["Authorization"] != "Bearer plain-bearer-token" {
		t.Errorf("Authorization = %q, want %q", headers["Authorization"], "Bearer plain-bearer-token")
	}
}

func TestApiHeaders_JWTAuth(t *testing.T) {
	// "abc.def" contains a dot, so it triggers JWT mode
	p := NewProvider("abc.def", "model")
	if p.authMode != "jwt" {
		t.Fatalf("expected jwt auth mode, got %q", p.authMode)
	}
	headers, err := p.apiHeaders()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	auth := headers["Authorization"]
	if !strings.HasPrefix(auth, "Bearer ") {
		t.Errorf("Authorization = %q, want Bearer prefix", auth)
	}
	token := strings.TrimPrefix(auth, "Bearer ")
	// JWT token should have 3 dot-separated parts (header.payload.signature)
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Errorf("JWT token should have 3 parts, got %d: %q", len(parts), token)
	}
}

func TestApiHeaders_JWTError(t *testing.T) {
	// JWT mode key always succeeds if it contains a dot — verify refresh works
	// after cache expiry by storing an expired entry.
	p := NewProvider("abc.def", "model")
	p.tokenCache.entries.Store("abc.def", cacheEntry{
		token:     "expired-token",
		expiresAt: time.Now().Add(-10 * time.Minute),
	})
	headers, err := p.apiHeaders()
	if err != nil {
		t.Fatalf("JWT refresh should succeed: %v", err)
	}
	auth := headers["Authorization"]
	if !strings.HasPrefix(auth, "Bearer ") {
		t.Errorf("Authorization = %q, want Bearer prefix", auth)
	}
	// Should be a newly generated token, not the expired one
	if strings.TrimPrefix(auth, "Bearer ") == "expired-token" {
		t.Error("should have refreshed the expired token")
	}
}

// --- ErrorResponse ---

func TestErrorResponseParsing(t *testing.T) {
	raw := `{"error":{"code":"401","message":"Invalid API key"}}`
	var errResp errorResponse
	if err := json.Unmarshal([]byte(raw), &errResp); err != nil {
		t.Fatalf("unmarshal error response: %v", err)
	}
	if errResp.Error.Code != "401" {
		t.Errorf("code = %q, want %q", errResp.Error.Code, "401")
	}
	if errResp.Error.Message != "Invalid API key" {
		t.Errorf("message = %q, want %q", errResp.Error.Message, "Invalid API key")
	}
}

// --- Provider Options ---

func TestWithThinkingEnabled(t *testing.T) {
	p := NewProvider("key", "model", WithThinkingEnabled())
	if p.thinking == nil {
		t.Fatal("thinking should not be nil")
	}
	if p.thinking.Type != "enabled" {
		t.Errorf("thinking.Type = %q, want %q", p.thinking.Type, "enabled")
	}
}

func TestWithClearThinking(t *testing.T) {
	p := NewProvider("key", "model", WithClearThinking(true))
	if p.thinking == nil {
		t.Fatal("thinking should not be nil (auto-enabled by WithClearThinking)")
	}
	if p.thinking.Type != "enabled" {
		t.Errorf("thinking.Type = %q, want %q", p.thinking.Type, "enabled")
	}
	if !p.thinking.ClearThinking {
		t.Error("thinking.ClearThinking = false, want true")
	}
}

func TestWithClearThinking_False(t *testing.T) {
	p := NewProvider("key", "model", WithClearThinking(false))
	if p.thinking == nil {
		t.Fatal("thinking should not be nil")
	}
	if p.thinking.ClearThinking {
		t.Error("thinking.ClearThinking = true, want false")
	}
}

func TestWithToolStream(t *testing.T) {
	p := NewProvider("key", "model", WithToolStream())
	if !p.toolStream {
		t.Error("toolStream = false, want true")
	}
}

func TestWithTopP(t *testing.T) {
	tests := []struct {
		name  string
		value float64
		want  float64
	}{
		{name: "within range", value: 0.9, want: 0.9},
		{name: "zero clamps to minimum", value: 0, want: 0.01},
		{name: "below minimum clamps to minimum", value: -1, want: 0.01},
		{name: "above maximum clamps to maximum", value: 2, want: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewProvider("key", "model", WithTopP(tt.value))
			if p.topP == nil || *p.topP != tt.want {
				t.Errorf("topP = %v, want %v", p.topP, tt.want)
			}
		})
	}
}

func TestWithDoSample(t *testing.T) {
	p := NewProvider("key", "model", WithDoSample(true))
	if p.doSample == nil {
		t.Fatal("doSample should not be nil")
	}
	if !*p.doSample {
		t.Error("doSample = false, want true")
	}
}

func TestWithRequestID(t *testing.T) {
	p := NewProvider("key", "model", WithRequestID("req-123"))
	if p.requestID != "req-123" {
		t.Errorf("requestID = %q, want %q", p.requestID, "req-123")
	}
}

func TestWithUserID(t *testing.T) {
	p := NewProvider("key", "model", WithUserID("user-456"))
	if p.userID != "user-456" {
		t.Errorf("userID = %q, want %q", p.userID, "user-456")
	}
}

// --- Chat Request Fields ---

func TestChatRequest_AllFieldsPopulated(t *testing.T) {
	sampleDoSample := true
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body = make([]byte, r.ContentLength)
		r.Body.Read(body)
		r.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	p := NewProvider("key", "model",
		WithBaseURL(srv.URL),
		WithTopP(0.8),
		WithDoSample(true),
		WithRequestID("req-abc"),
		WithUserID("user-xyz"),
		WithThinkingEnabled(),
		WithLogger(nil),
	)
	p.logger = newTestLogger()

	_, _, err := p.Chat(context.Background(), []llm.Message{llm.UserMessage("hi")}, nil,
		llm.WithMaxTokens(100),
		llm.WithTemperature(0.5),
		llm.WithStop("END"),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshal request body: %v", err)
	}

	if req["top_p"] != 0.8 {
		t.Errorf("top_p = %v, want 0.8", req["top_p"])
	}
	if req["do_sample"] != true {
		t.Errorf("do_sample = %v, want true", req["do_sample"])
	}
	if req["request_id"] != "req-abc" {
		t.Errorf("request_id = %v, want req-abc", req["request_id"])
	}
	if req["user_id"] != "user-xyz" {
		t.Errorf("user_id = %v, want user-xyz", req["user_id"])
	}
	if req["max_tokens"] != float64(100) {
		t.Errorf("max_tokens = %v, want 100", req["max_tokens"])
	}
	if req["temperature"] != 0.5 {
		t.Errorf("temperature = %v, want 0.5", req["temperature"])
	}

	thinking, ok := req["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("thinking = %T, want map", req["thinking"])
	}
	if thinking["type"] != "enabled" {
		t.Errorf("thinking.type = %v, want enabled", thinking["type"])
	}

	stop, ok := req["stop"].([]any)
	if !ok {
		t.Fatalf("stop = %T, want array", req["stop"])
	}
	if len(stop) != 1 || stop[0] != "END" {
		t.Errorf("stop = %v, want [END]", stop)
	}

	_ = sampleDoSample
}

func TestChatRequest_StopFieldTruncated(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body = make([]byte, r.ContentLength)
		r.Body.Read(body)
		r.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	p := NewProvider("key", "model",
		WithBaseURL(srv.URL),
		WithLogger(newTestLogger()),
	)

	_, _, err := p.Chat(context.Background(), []llm.Message{llm.UserMessage("hi")}, nil,
		llm.WithStop("STOP1", "STOP2", "STOP3"),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshal request body: %v", err)
	}

	stop, ok := req["stop"].([]any)
	if !ok {
		t.Fatalf("stop = %T, want array", req["stop"])
	}
	if len(stop) != 1 {
		t.Errorf("stop has %d items, want 1 (truncated)", len(stop))
	}
	if stop[0] != "STOP1" {
		t.Errorf("stop[0] = %v, want STOP1", stop[0])
	}
}

// --- ChatStream Request Fields ---

func TestChatStreamRequest_ToolStreamEnabled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		var body []byte
		body = make([]byte, r.ContentLength)
		r.Body.Read(body)
		r.Body.Close()
		json.Unmarshal(body, &req)

		w.Header().Set("Content-Type", "text/event-stream")
		// Verify tool_stream was set
		if req["tool_stream"] != true {
			t.Errorf("tool_stream = %v, want true", req["tool_stream"])
		}
		if req["stream"] != true {
			t.Errorf("stream = %v, want true", req["stream"])
		}
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	p := NewProvider("key", "model",
		WithBaseURL(srv.URL),
		WithToolStream(),
		WithLogger(newTestLogger()),
	)

	ctx := context.Background()
	iter, err := p.ChatStream(ctx, []llm.Message{llm.UserMessage("hi")}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for range iter {
	}
}

func TestChatStreamRequest_ToolStreamWithoutStreamGuard(t *testing.T) {
	// Without WithToolStream, tool_stream should not be set
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		var body []byte
		body = make([]byte, r.ContentLength)
		r.Body.Read(body)
		r.Body.Close()
		json.Unmarshal(body, &req)

		if _, ok := req["tool_stream"]; ok {
			t.Error("tool_stream should not be set when WithToolStream is not used")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	p := NewProvider("key", "model",
		WithBaseURL(srv.URL),
		WithLogger(newTestLogger()),
	)

	ctx := context.Background()
	iter, err := p.ChatStream(ctx, []llm.Message{llm.UserMessage("hi")}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for range iter {
	}
}

func TestChatStream_MalformedPayloadReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {not-json}\n\n")
	}))
	defer srv.Close()

	p := NewProvider("key", "model",
		WithBaseURL(srv.URL),
		WithLogger(newTestLogger()),
	)

	stream, err := p.ChatStream(context.Background(), []llm.Message{llm.UserMessage("hi")}, nil)
	if err != nil {
		t.Fatalf("ChatStream() setup error = %v", err)
	}

	var streamErr error
	for _, err := range stream {
		if err != nil {
			streamErr = err
		}
	}
	if streamErr == nil {
		t.Fatal("ChatStream() error = nil, want malformed payload error")
	}
}

// --- convertResponseMessage — ReasoningBlock ---

func TestConvertResponseMessage_ReasoningBlock(t *testing.T) {
	reasoning := "I need to think about this step by step..."
	content := "Here is the answer"
	msg, err := convertResponseMessage(respMessage{
		Role:             "assistant",
		ReasoningContent: &reasoning,
		Content:          &content,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(msg.Content) != 2 {
		t.Fatalf("expected 2 content blocks, got %d", len(msg.Content))
	}
	rb, ok := msg.Content[0].(llm.ReasoningBlock)
	if !ok {
		t.Fatalf("expected ReasoningBlock at [0], got %T", msg.Content[0])
	}
	if rb.Type != "reasoning" {
		t.Errorf("ReasoningBlock.Type = %q, want %q", rb.Type, "reasoning")
	}
	if rb.Content != reasoning {
		t.Errorf("ReasoningBlock.Content = %q, want %q", rb.Content, reasoning)
	}
	tb, ok := msg.Content[1].(llm.TextBlock)
	if !ok {
		t.Fatalf("expected TextBlock at [1], got %T", msg.Content[1])
	}
	if tb.Text != content {
		t.Errorf("TextBlock.Text = %q, want %q", tb.Text, content)
	}
}

func TestConvertResponseMessage_NoReasoningBlock(t *testing.T) {
	content := "just text"
	msg, err := convertResponseMessage(respMessage{
		Role:    "assistant",
		Content: &content,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(msg.Content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(msg.Content))
	}
	if _, ok := msg.Content[0].(llm.ReasoningBlock); ok {
		t.Error("should not have ReasoningBlock when no reasoning_content")
	}
}

func TestConvertResponseMessage_EmptyReasoningContent(t *testing.T) {
	empty := ""
	content := "the answer"
	msg, err := convertResponseMessage(respMessage{
		Role:             "assistant",
		ReasoningContent: &empty,
		Content:          &content,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Empty reasoning should NOT produce a ReasoningBlock
	if len(msg.Content) != 1 {
		t.Fatalf("expected 1 content block (reasoning was empty), got %d", len(msg.Content))
	}
	if _, ok := msg.Content[0].(llm.ReasoningBlock); ok {
		t.Error("empty reasoning_content should not produce ReasoningBlock")
	}
}

func TestConvertResponseMessage_ReasoningWithToolCalls(t *testing.T) {
	reasoning := "Let me analyze this..."
	msg, err := convertResponseMessage(respMessage{
		Role:             "assistant",
		ReasoningContent: &reasoning,
		ToolCalls: []toolCall{
			{
				ID:   "call_1",
				Type: "function",
				Function: functionCall{
					Name:      "search",
					Arguments: `{"q":"test"}`,
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should have ReasoningBlock + ToolUseBlock
	if len(msg.Content) != 2 {
		t.Fatalf("expected 2 content blocks, got %d", len(msg.Content))
	}
	if _, ok := msg.Content[0].(llm.ReasoningBlock); !ok {
		t.Errorf("expected ReasoningBlock at [0], got %T", msg.Content[0])
	}
	if _, ok := msg.Content[1].(llm.ToolUseBlock); !ok {
		t.Errorf("expected ToolUseBlock at [1], got %T", msg.Content[1])
	}
}

func TestConvertResponseMessage_NilContentWithReasoning(t *testing.T) {
	reasoning := "thinking deeply..."
	msg, err := convertResponseMessage(respMessage{
		Role:             "assistant",
		Content:          nil,
		ReasoningContent: &reasoning,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(msg.Content) != 1 {
		t.Fatalf("expected 1 content block (reasoning only), got %d", len(msg.Content))
	}
	rb, ok := msg.Content[0].(llm.ReasoningBlock)
	if !ok {
		t.Fatalf("expected ReasoningBlock, got %T", msg.Content[0])
	}
	if rb.Content != reasoning {
		t.Errorf("content = %q, want %q", rb.Content, reasoning)
	}
}

func TestChat_ReasoningOnlyResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","reasoning_content":"I thought about it"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	p := NewProvider("key", "model",
		WithBaseURL(srv.URL),
		WithLogger(newTestLogger()),
	)

	msg, _, err := p.Chat(context.Background(), []llm.Message{llm.UserMessage("think")}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(msg.Content) != 1 {
		t.Fatalf("expected 1 content block (reasoning only), got %d", len(msg.Content))
	}
	rb, ok := msg.Content[0].(llm.ReasoningBlock)
	if !ok {
		t.Fatalf("expected ReasoningBlock, got %T", msg.Content[0])
	}
	if rb.Content != "I thought about it" {
		t.Errorf("content = %q, want %q", rb.Content, "I thought about it")
	}
}

// --- GLM-specific finish reasons (pass through verbatim) ---

func TestConvertResponseMessage_SensitiveFinish(t *testing.T) {
	content := "I can't help with that"
	msg, err := convertResponseMessage(respMessage{
		Role:    "assistant",
		Content: &content,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Just ensure it doesn't crash — finish_reason is at the choice level, not respMessage
	if len(msg.Content) != 1 {
		t.Fatalf("expected 1 block, got %d", len(msg.Content))
	}
}

func TestConvertResponseMessage_ContextExceeded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"partial response"},"finish_reason":"model_context_window_exceeded"}]}`)
	}))
	defer srv.Close()

	p := NewProvider("key", "model", WithBaseURL(srv.URL), WithLogger(newTestLogger()))
	_, _, err := p.Chat(context.Background(), []llm.Message{llm.UserMessage("hi")}, nil)
	if err == nil {
		t.Fatal("Chat() error = nil, want context window error")
	}
	if !strings.Contains(err.Error(), "model_context_window_exceeded") {
		t.Errorf("Chat() error = %q, want finish reason", err)
	}
	if llm.IsRetryableError(err) {
		t.Errorf("Chat() error = %v, want non-retryable", err)
	}
}

func TestConvertResponseMessage_NetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"partial response"},"finish_reason":"network_error"}]}`)
	}))
	defer srv.Close()

	p := NewProvider("key", "model", WithBaseURL(srv.URL), WithLogger(newTestLogger()))
	_, _, err := p.Chat(context.Background(), []llm.Message{llm.UserMessage("hi")}, nil)
	if err == nil {
		t.Fatal("Chat() error = nil, want network finish error")
	}
	if !strings.Contains(err.Error(), "network_error") {
		t.Errorf("Chat() error = %q, want finish reason", err)
	}
	if !llm.IsRetryableError(err) {
		t.Errorf("Chat() error = %v, want retryable", err)
	}
}

func TestChatStream_FailureFinishReasonReturnsError(t *testing.T) {
	tests := []struct {
		name      string
		reason    string
		retryable bool
	}{
		{name: "network error", reason: "network_error", retryable: true},
		{name: "context exceeded", reason: "model_context_window_exceeded", retryable: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"},\"finish_reason\":%q}]}\n\n", tt.reason)
			}))
			defer srv.Close()

			p := NewProvider("key", "model", WithBaseURL(srv.URL), WithLogger(newTestLogger()))
			stream, err := p.ChatStream(context.Background(), []llm.Message{llm.UserMessage("hi")}, nil)
			if err != nil {
				t.Fatalf("ChatStream() setup error = %v", err)
			}

			var streamErr error
			for chunk, err := range stream {
				if _, ok := chunk.(llm.DoneChunk); ok {
					t.Error("ChatStream() yielded DoneChunk for failure finish reason")
				}
				if err != nil {
					streamErr = err
				}
			}
			if streamErr == nil {
				t.Fatal("ChatStream() error = nil, want finish reason error")
			}
			if !strings.Contains(streamErr.Error(), tt.reason) {
				t.Errorf("ChatStream() error = %q, want finish reason", streamErr)
			}
			if got := llm.IsRetryableError(streamErr); got != tt.retryable {
				t.Errorf("IsRetryableError() = %v, want %v", got, tt.retryable)
			}
		})
	}
}

// --- ReasoningBlock stripped in outbound conversions ---

func TestConvertAssistant_ReasoningBlockStripped(t *testing.T) {
	msg := llm.Message{
		Role: llm.RoleAssistant,
		Content: []llm.ContentBlock{
			llm.ReasoningBlock{Type: "reasoning", Content: "I'm thinking..."},
			llm.TextBlock{Type: "text", Text: "Here's the answer"},
		},
	}
	cm, err := convertAssistant(msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// ReasoningBlock should be silently stripped, only text remains
	s, ok := cm.Content.(string)
	if !ok {
		t.Fatalf("expected string content, got %T", cm.Content)
	}
	if s != "Here's the answer" {
		t.Errorf("content = %q, want %q", s, "Here's the answer")
	}
}

func TestConvertStandard_ReasoningBlockStripped(t *testing.T) {
	msg := llm.Message{
		Role: llm.RoleUser,
		Content: []llm.ContentBlock{
			llm.ReasoningBlock{Type: "reasoning", Content: "should be stripped"},
			llm.TextBlock{Type: "text", Text: "actual user text"},
		},
	}
	cm, err := convertStandardMessage(msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// ReasoningBlock should be silently stripped
	parts, ok := cm.Content.([]contentPart)
	if !ok {
		t.Fatalf("expected []contentPart, got %T", cm.Content)
	}
	if len(parts) != 1 {
		t.Fatalf("expected 1 part, got %d", len(parts))
	}
	if parts[0].Text != "actual user text" {
		t.Errorf("text = %q, want %q", parts[0].Text, "actual user text")
	}
}

// --- Helpers ---

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&discardWriter{}, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

type discardWriter struct{}

func (d *discardWriter) Write(p []byte) (int, error) { return len(p), nil }
