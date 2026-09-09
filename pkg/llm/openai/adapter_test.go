package openai

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/pkg/llm"
	"github.com/dailz1/go-agent/pkg/tool"
)

func strPtr(s string) *string { return &s }

// --- convertStandardMessage ---

func TestConvertStandardMessage_SingleText(t *testing.T) {
	msg := llm.Message{
		Role:    llm.RoleUser,
		Content: []llm.ContentBlock{llm.TextBlock{Type: "text", Text: "hello"}},
	}

	got, err := convertStandardMessage(msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got.Role != "user" {
		t.Errorf("Role = %q, want %q", got.Role, "user")
	}
	str, ok := got.Content.(string)
	if !ok {
		t.Fatalf("Content type = %T, want string", got.Content)
	}
	if str != "hello" {
		t.Errorf("Content = %q, want %q", str, "hello")
	}
}

func TestConvertStandardMessage_MultipleBlocks(t *testing.T) {
	msg := llm.Message{
		Role: llm.RoleUser,
		Content: []llm.ContentBlock{
			llm.TextBlock{Type: "text", Text: "first"},
			llm.TextBlock{Type: "text", Text: "second"},
		},
	}

	got, err := convertStandardMessage(msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	parts, ok := got.Content.([]contentPart)
	if !ok {
		t.Fatalf("Content type = %T, want []contentPart", got.Content)
	}
	if len(parts) != 2 {
		t.Fatalf("len(parts) = %d, want 2", len(parts))
	}
	for i, want := range []string{"first", "second"} {
		if parts[i].Type != "text" {
			t.Errorf("parts[%d].Type = %q, want %q", i, parts[i].Type, "text")
		}
		if parts[i].Text != want {
			t.Errorf("parts[%d].Text = %q, want %q", i, parts[i].Text, want)
		}
	}
}

func TestConvertStandardMessage_ImageBlock(t *testing.T) {
	msg := llm.Message{
		Role: llm.RoleUser,
		Content: []llm.ContentBlock{
			llm.ImageBlock{Type: "image", URL: "https://example.com/img.png"},
		},
	}

	got, err := convertStandardMessage(msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	parts, ok := got.Content.([]contentPart)
	if !ok {
		t.Fatalf("Content type = %T, want []contentPart", got.Content)
	}
	if len(parts) != 1 {
		t.Fatalf("len(parts) = %d, want 1", len(parts))
	}
	p := parts[0]
	if p.Type != "image_url" {
		t.Errorf("Type = %q, want %q", p.Type, "image_url")
	}
	if p.ImageURL == nil {
		t.Fatal("ImageURL is nil")
	}
	if p.ImageURL.URL != "https://example.com/img.png" {
		t.Errorf("ImageURL.URL = %q, want %q", p.ImageURL.URL, "https://example.com/img.png")
	}
}

func TestConvertStandardMessage_ImageBlockBase64(t *testing.T) {
	msg := llm.Message{
		Role: llm.RoleUser,
		Content: []llm.ContentBlock{
			llm.ImageBlock{Type: "image", MIMEType: "image/png", Data: []byte("fake-png-data")},
		},
	}

	got, err := convertStandardMessage(msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	parts, ok := got.Content.([]contentPart)
	if !ok {
		t.Fatalf("Content type = %T, want []contentPart", got.Content)
	}
	if len(parts) != 1 {
		t.Fatalf("len(parts) = %d, want 1", len(parts))
	}
	p := parts[0]
	if p.ImageURL == nil {
		t.Fatal("ImageURL is nil")
	}
	want := "data:image/png;base64," + encodeBase64([]byte("fake-png-data"))
	if p.ImageURL.URL != want {
		t.Errorf("ImageURL.URL = %q, want %q", p.ImageURL.URL, want)
	}
}

// --- convertAssistant ---

func TestConvertAssistant_TextOnly(t *testing.T) {
	msg := llm.Message{
		Role:    llm.RoleAssistant,
		Content: []llm.ContentBlock{llm.TextBlock{Type: "text", Text: "response"}},
	}

	got, err := convertAssistant(msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got.Role != "assistant" {
		t.Errorf("Role = %q, want %q", got.Role, "assistant")
	}
	str, ok := got.Content.(string)
	if !ok {
		t.Fatalf("Content type = %T, want string", got.Content)
	}
	if str != "response" {
		t.Errorf("Content = %q, want %q", str, "response")
	}
	if len(got.ToolCalls) != 0 {
		t.Errorf("ToolCalls len = %d, want 0", len(got.ToolCalls))
	}
}

func TestConvertAssistant_ToolCallsOnly(t *testing.T) {
	input := json.RawMessage(`{"x":1}`)
	msg := llm.Message{
		Role: llm.RoleAssistant,
		Content: []llm.ContentBlock{
			llm.ToolUseBlock{Type: "tool_use", ID: "c1", Name: "fn", Input: input},
		},
	}

	got, err := convertAssistant(msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got.Content != "" {
		t.Errorf("Content = %v, want empty string", got.Content)
	}
	if len(got.ToolCalls) != 1 {
		t.Fatalf("ToolCalls len = %d, want 1", len(got.ToolCalls))
	}
	tc := got.ToolCalls[0]
	if tc.ID != "c1" {
		t.Errorf("ToolCalls[0].ID = %q, want %q", tc.ID, "c1")
	}
	if tc.Type != "function" {
		t.Errorf("ToolCalls[0].Type = %q, want %q", tc.Type, "function")
	}
	if tc.Function.Name != "fn" {
		t.Errorf("ToolCalls[0].Function.Name = %q, want %q", tc.Function.Name, "fn")
	}
	if tc.Function.Arguments != `{"x":1}` {
		t.Errorf("ToolCalls[0].Function.Arguments = %q, want %q", tc.Function.Arguments, `{"x":1}`)
	}
}

func TestConvertAssistant_MixedContent(t *testing.T) {
	input := json.RawMessage(`{"a":2}`)
	msg := llm.Message{
		Role: llm.RoleAssistant,
		Content: []llm.ContentBlock{
			llm.TextBlock{Type: "text", Text: "partial answer"},
			llm.ToolUseBlock{Type: "tool_use", ID: "c2", Name: "search", Input: input},
		},
	}

	got, err := convertAssistant(msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	str, ok := got.Content.(string)
	if !ok {
		t.Fatalf("Content type = %T, want string", got.Content)
	}
	if str != "partial answer" {
		t.Errorf("Content = %q, want %q", str, "partial answer")
	}
	if len(got.ToolCalls) != 1 {
		t.Fatalf("ToolCalls len = %d, want 1", len(got.ToolCalls))
	}
	if got.ToolCalls[0].Function.Name != "search" {
		t.Errorf("ToolCalls[0].Function.Name = %q, want %q", got.ToolCalls[0].Function.Name, "search")
	}
}

func TestConvertAssistant_MultipleTextParts(t *testing.T) {
	msg := llm.Message{
		Role: llm.RoleAssistant,
		Content: []llm.ContentBlock{
			llm.TextBlock{Type: "text", Text: "line1"},
			llm.TextBlock{Type: "text", Text: "line2"},
		},
	}

	got, err := convertAssistant(msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	str, ok := got.Content.(string)
	if !ok {
		t.Fatalf("Content type = %T, want string", got.Content)
	}
	want := "line1\nline2"
	if str != want {
		t.Errorf("Content = %q, want %q", str, want)
	}
}

// --- convertToolResult ---

func TestConvertToolResult(t *testing.T) {
	msg := llm.Message{
		Role: llm.RoleTool,
		Content: []llm.ContentBlock{
			llm.ToolResultBlock{Type: "tool_result", ToolUseID: "id1", Content: "result text"},
		},
	}

	got, err := convertToolResult(msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got.Role != "tool" {
		t.Errorf("Role = %q, want %q", got.Role, "tool")
	}
	if got.ToolCallID != "id1" {
		t.Errorf("ToolCallID = %q, want %q", got.ToolCallID, "id1")
	}
	str, ok := got.Content.(string)
	if !ok {
		t.Fatalf("Content type = %T, want string", got.Content)
	}
	if str != "result text" {
		t.Errorf("Content = %q, want %q", str, "result text")
	}
}

func TestConvertToolResult_NoBlock(t *testing.T) {
	msg := llm.Message{
		Role:    llm.RoleTool,
		Content: []llm.ContentBlock{llm.TextBlock{Type: "text", Text: "not a result"}},
	}

	_, err := convertToolResult(msg)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

// --- convertToolDefs ---

func TestConvertToolDefs(t *testing.T) {
	tools := []tool.ToolInfo{
		{
			Name:        "get_weather",
			Description: "Get current weather",
			Parameters: tool.ParameterSchema{
				Type: "object",
				Properties: map[string]tool.Property{
					"city": {Type: "string", Description: "City name"},
				},
				Required: []string{"city"},
			},
		},
		{
			Name:        "search",
			Description: "Search the web",
			Parameters: tool.ParameterSchema{
				Type: "object",
				Properties: map[string]tool.Property{
					"query": {Type: "string", Description: "Search query"},
				},
			},
		},
	}

	got, err := convertToolDefs(tools)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}

	for i, want := range []struct {
		name, desc string
	}{
		{"get_weather", "Get current weather"},
		{"search", "Search the web"},
	} {
		if got[i].Type != "function" {
			t.Errorf("got[%d].Type = %q, want %q", i, got[i].Type, "function")
		}
		if got[i].Function.Name != want.name {
			t.Errorf("got[%d].Function.Name = %q, want %q", i, got[i].Function.Name, want.name)
		}
		if got[i].Function.Description != want.desc {
			t.Errorf("got[%d].Function.Description = %q, want %q", i, got[i].Function.Description, want.desc)
		}
		if len(got[i].Function.Parameters) == 0 {
			t.Errorf("got[%d].Function.Parameters is empty", i)
		}
		var parsed map[string]any
		if err := json.Unmarshal(got[i].Function.Parameters, &parsed); err != nil {
			t.Errorf("got[%d].Function.Parameters is not valid JSON: %v", i, err)
		}
	}
}

// --- convertResponseMessage ---

func TestConvertResponseMessage_TextOnly(t *testing.T) {
	rm := respMessage{
		Role:    "assistant",
		Content: strPtr("hello"),
	}

	got, err := convertResponseMessage(rm)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got.Role != llm.RoleAssistant {
		t.Errorf("Role = %q, want %q", got.Role, llm.RoleAssistant)
	}
	if len(got.Content) != 1 {
		t.Fatalf("len(Content) = %d, want 1", len(got.Content))
	}
	tb, ok := got.Content[0].(llm.TextBlock)
	if !ok {
		t.Fatalf("Content[0] type = %T, want TextBlock", got.Content[0])
	}
	if tb.Text != "hello" {
		t.Errorf("TextBlock.Text = %q, want %q", tb.Text, "hello")
	}
}

func TestConvertResponseMessage_ToolCalls(t *testing.T) {
	rm := respMessage{
		Role:    "assistant",
		Content: nil,
		ToolCalls: []toolCall{
			{ID: "tc1", Type: "function", Function: functionCall{Name: "run", Arguments: `{"p":1}`}},
		},
	}

	got, err := convertResponseMessage(rm)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(got.Content) != 1 {
		t.Fatalf("len(Content) = %d, want 1", len(got.Content))
	}
	tub, ok := got.Content[0].(llm.ToolUseBlock)
	if !ok {
		t.Fatalf("Content[0] type = %T, want ToolUseBlock", got.Content[0])
	}
	if tub.ID != "tc1" {
		t.Errorf("ID = %q, want %q", tub.ID, "tc1")
	}
	if tub.Name != "run" {
		t.Errorf("Name = %q, want %q", tub.Name, "run")
	}
	if !reflect.DeepEqual(tub.Input, json.RawMessage(`{"p":1}`)) {
		t.Errorf("Input = %s, want %s", tub.Input, `{"p":1}`)
	}
}

func TestConvertResponseMessage_Mixed(t *testing.T) {
	rm := respMessage{
		Role:    "assistant",
		Content: strPtr("thinking"),
		ToolCalls: []toolCall{
			{ID: "tc2", Type: "function", Function: functionCall{Name: "act", Arguments: `{}`}},
		},
	}

	got, err := convertResponseMessage(rm)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(got.Content) != 2 {
		t.Fatalf("len(Content) = %d, want 2", len(got.Content))
	}

	tb, ok := got.Content[0].(llm.TextBlock)
	if !ok {
		t.Fatalf("Content[0] type = %T, want TextBlock", got.Content[0])
	}
	if tb.Text != "thinking" {
		t.Errorf("TextBlock.Text = %q, want %q", tb.Text, "thinking")
	}

	tub, ok := got.Content[1].(llm.ToolUseBlock)
	if !ok {
		t.Fatalf("Content[1] type = %T, want ToolUseBlock", got.Content[1])
	}
	if tub.Name != "act" {
		t.Errorf("ToolUseBlock.Name = %q, want %q", tub.Name, "act")
	}
}

func TestConvertResponseMessage_Empty(t *testing.T) {
	rm := respMessage{
		Role:      "assistant",
		Content:   nil,
		ToolCalls: nil,
	}

	_, err := convertResponseMessage(rm)
	if err == nil {
		t.Fatal("expected error for empty response message, got nil")
	}
}

// --- convertStandardMessage edge cases ---

func TestConvertStandardMessage_EmptyContent(t *testing.T) {
	t.Parallel()
	msg := llm.Message{
		Role:    llm.RoleUser,
		Content: []llm.ContentBlock{},
	}

	got, err := convertStandardMessage(msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Role != "user" {
		t.Errorf("Role = %q, want %q", got.Role, "user")
	}
	parts, ok := got.Content.([]contentPart)
	if !ok {
		t.Fatalf("Content type = %T, want []contentPart", got.Content)
	}
	if len(parts) != 0 {
		t.Errorf("len(parts) = %d, want 0", len(parts))
	}
}

func TestConvertStandardMessage_NilContent(t *testing.T) {
	t.Parallel()
	msg := llm.Message{
		Role:    llm.RoleUser,
		Content: nil,
	}

	got, err := convertStandardMessage(msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Role != "user" {
		t.Errorf("Role = %q, want %q", got.Role, "user")
	}
	parts, ok := got.Content.([]contentPart)
	if !ok {
		t.Fatalf("Content type = %T, want []contentPart", got.Content)
	}
	if len(parts) != 0 {
		t.Errorf("len(parts) = %d, want 0", len(parts))
	}
}

func TestConvertStandardMessage_UnsupportedBlock(t *testing.T) {
	t.Parallel()
	msg := llm.Message{
		Role: llm.RoleUser,
		Content: []llm.ContentBlock{
			llm.ToolUseBlock{Type: "tool_use", ID: "bad", Name: "fn", Input: json.RawMessage(`{}`)},
		},
	}

	_, err := convertStandardMessage(msg)
	if err == nil {
		t.Fatal("expected error for unsupported block type in user role, got nil")
	}
}

func TestConvertStandardMessage_ImageBlockEmptyURL(t *testing.T) {
	t.Parallel()
	msg := llm.Message{
		Role: llm.RoleUser,
		Content: []llm.ContentBlock{
			llm.ImageBlock{Type: "image", URL: ""},
		},
	}

	got, err := convertStandardMessage(msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	parts, ok := got.Content.([]contentPart)
	if !ok {
		t.Fatalf("Content type = %T, want []contentPart", got.Content)
	}
	if len(parts) != 1 {
		t.Fatalf("len(parts) = %d, want 1", len(parts))
	}
	if parts[0].Type != "image_url" {
		t.Errorf("Type = %q, want %q", parts[0].Type, "image_url")
	}
	if parts[0].ImageURL == nil {
		t.Fatal("ImageURL is nil")
	}
	if parts[0].ImageURL.URL != "" {
		t.Errorf("ImageURL.URL = %q, want empty string", parts[0].ImageURL.URL)
	}
}

// --- convertAssistant edge cases ---

func TestConvertAssistant_EmptyContent(t *testing.T) {
	t.Parallel()
	msg := llm.Message{
		Role:    llm.RoleAssistant,
		Content: []llm.ContentBlock{},
	}

	got, err := convertAssistant(msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Content != "" {
		t.Errorf("Content = %v, want empty string", got.Content)
	}
	if got.ToolCalls != nil {
		t.Errorf("ToolCalls = %v, want nil", got.ToolCalls)
	}
}

func TestConvertAssistant_UnsupportedBlock(t *testing.T) {
	t.Parallel()
	msg := llm.Message{
		Role: llm.RoleAssistant,
		Content: []llm.ContentBlock{
			llm.ImageBlock{Type: "image", URL: "https://example.com/img.png"},
		},
	}

	_, err := convertAssistant(msg)
	if err == nil {
		t.Fatal("expected error for unsupported block type in assistant message, got nil")
	}
}

func TestConvertAssistant_SingleEmptyText(t *testing.T) {
	t.Parallel()
	msg := llm.Message{
		Role:    llm.RoleAssistant,
		Content: []llm.ContentBlock{llm.TextBlock{Type: "text", Text: ""}},
	}

	got, err := convertAssistant(msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	str, ok := got.Content.(string)
	if !ok {
		t.Fatalf("Content type = %T, want string", got.Content)
	}
	if str != "" {
		t.Errorf("Content = %q, want empty string", str)
	}
	if len(got.ToolCalls) != 0 {
		t.Errorf("ToolCalls len = %d, want 0", len(got.ToolCalls))
	}
}

// --- convertToolResult edge cases ---

func TestConvertToolResult_ErrorResult(t *testing.T) {
	t.Parallel()
	msg := llm.Message{
		Role: llm.RoleTool,
		Content: []llm.ContentBlock{
			llm.ToolResultBlock{
				Type:      "tool_result",
				ToolUseID: "err1",
				Content:   "something went wrong",
				IsError:   true,
			},
		},
	}

	got, err := convertToolResult(msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Role != "tool" {
		t.Errorf("Role = %q, want %q", got.Role, "tool")
	}
	if got.ToolCallID != "err1" {
		t.Errorf("ToolCallID = %q, want %q", got.ToolCallID, "err1")
	}
	str, ok := got.Content.(string)
	if !ok {
		t.Fatalf("Content type = %T, want string", got.Content)
	}
	if str != "something went wrong" {
		t.Errorf("Content = %q, want %q", str, "something went wrong")
	}
}

func TestConvertToolResult_EmptyContent(t *testing.T) {
	t.Parallel()
	msg := llm.Message{
		Role: llm.RoleTool,
		Content: []llm.ContentBlock{
			llm.ToolResultBlock{
				Type:      "tool_result",
				ToolUseID: "empty1",
				Content:   "",
			},
		},
	}

	got, err := convertToolResult(msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ToolCallID != "empty1" {
		t.Errorf("ToolCallID = %q, want %q", got.ToolCallID, "empty1")
	}
	str, ok := got.Content.(string)
	if !ok {
		t.Fatalf("Content type = %T, want string", got.Content)
	}
	if str != "" {
		t.Errorf("Content = %q, want empty string", str)
	}
}

func TestConvertToolResult_MultipleToolResultBlocks(t *testing.T) {
	t.Parallel()
	msg := llm.Message{
		Role: llm.RoleTool,
		Content: []llm.ContentBlock{
			llm.ToolResultBlock{
				Type:      "tool_result",
				ToolUseID: "first",
				Content:   "result one",
			},
			llm.ToolResultBlock{
				Type:      "tool_result",
				ToolUseID: "second",
				Content:   "result two",
			},
		},
	}

	got, err := convertToolResult(msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ToolCallID != "first" {
		t.Errorf("ToolCallID = %q, want %q", got.ToolCallID, "first")
	}
	str, ok := got.Content.(string)
	if !ok {
		t.Fatalf("Content type = %T, want string", got.Content)
	}
	if str != "result one" {
		t.Errorf("Content = %q, want %q", str, "result one")
	}
}

// --- convertResponseMessage edge cases ---

func TestConvertResponseMessage_ContentEmptyString(t *testing.T) {
	t.Parallel()
	rm := respMessage{
		Role:    "assistant",
		Content: strPtr(""),
	}

	_, err := convertResponseMessage(rm)
	// Empty string content is intentionally skipped (*rm.Content != "" check),
	// so with no tool calls this should return an error.
	if err == nil {
		t.Fatal("expected error for empty string content with no tool calls, got nil")
	}
}

func TestConvertResponseMessage_NilContentWithToolCalls(t *testing.T) {
	t.Parallel()
	rm := respMessage{
		Role:    "assistant",
		Content: nil,
		ToolCalls: []toolCall{
			{ID: "tc_n1", Type: "function", Function: functionCall{Name: "fetch", Arguments: `{"url":"x"}`}},
		},
	}

	got, err := convertResponseMessage(rm)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Content) != 1 {
		t.Fatalf("len(Content) = %d, want 1", len(got.Content))
	}
	tub, ok := got.Content[0].(llm.ToolUseBlock)
	if !ok {
		t.Fatalf("Content[0] type = %T, want ToolUseBlock", got.Content[0])
	}
	if tub.ID != "tc_n1" {
		t.Errorf("ID = %q, want %q", tub.ID, "tc_n1")
	}
	if tub.Name != "fetch" {
		t.Errorf("Name = %q, want %q", tub.Name, "fetch")
	}
}

func TestConvertResponseMessage_MultipleToolCalls(t *testing.T) {
	t.Parallel()
	rm := respMessage{
		Role:    "assistant",
		Content: nil,
		ToolCalls: []toolCall{
			{ID: "mc1", Type: "function", Function: functionCall{Name: "fn1", Arguments: `{"a":1}`}},
			{ID: "mc2", Type: "function", Function: functionCall{Name: "fn2", Arguments: `{"b":2}`}},
			{ID: "mc3", Type: "function", Function: functionCall{Name: "fn3", Arguments: `{"c":3}`}},
		},
	}

	got, err := convertResponseMessage(rm)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Content) != 3 {
		t.Fatalf("len(Content) = %d, want 3", len(got.Content))
	}

	wantNames := []string{"fn1", "fn2", "fn3"}
	wantIDs := []string{"mc1", "mc2", "mc3"}
	for i, block := range got.Content {
		tub, ok := block.(llm.ToolUseBlock)
		if !ok {
			t.Fatalf("Content[%d] type = %T, want ToolUseBlock", i, block)
		}
		if tub.ID != wantIDs[i] {
			t.Errorf("Content[%d].ID = %q, want %q", i, tub.ID, wantIDs[i])
		}
		if tub.Name != wantNames[i] {
			t.Errorf("Content[%d].Name = %q, want %q", i, tub.Name, wantNames[i])
		}
	}
}

func TestConvertResponseMessage_EmptyArguments(t *testing.T) {
	t.Parallel()
	rm := respMessage{
		Role:    "assistant",
		Content: strPtr("text"),
		ToolCalls: []toolCall{
			{ID: "ea1", Type: "function", Function: functionCall{Name: "fn", Arguments: ""}},
		},
	}

	got, err := convertResponseMessage(rm)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Content) != 2 {
		t.Fatalf("len(Content) = %d, want 2", len(got.Content))
	}

	tb, ok := got.Content[0].(llm.TextBlock)
	if !ok {
		t.Fatalf("Content[0] type = %T, want TextBlock", got.Content[0])
	}
	if tb.Text != "text" {
		t.Errorf("Text = %q, want %q", tb.Text, "text")
	}

	tub, ok := got.Content[1].(llm.ToolUseBlock)
	if !ok {
		t.Fatalf("Content[1] type = %T, want ToolUseBlock", got.Content[1])
	}
	if !reflect.DeepEqual(tub.Input, json.RawMessage("")) {
		t.Errorf("Input = %q, want empty string", string(tub.Input))
	}
}

// --- convertToolDefs edge cases ---

func TestConvertToolDefs_Empty(t *testing.T) {
	t.Parallel()
	got, err := convertToolDefs([]tool.ToolInfo{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len(got) = %d, want 0", len(got))
	}
}

func TestConvertToolDefs_RequiresApproval(t *testing.T) {
	t.Parallel()
	tools := []tool.ToolInfo{
		{
			Name:             "dangerous_op",
			Description:      "A tool requiring approval",
			RequiresApproval: true,
			Parameters:       tool.NewParameterSchema(),
		},
	}

	got, err := convertToolDefs(tools)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}

	def := got[0]
	if def.Type != "function" {
		t.Errorf("Type = %q, want %q", def.Type, "function")
	}
	if def.Function.Name != "dangerous_op" {
		t.Errorf("Name = %q, want %q", def.Function.Name, "dangerous_op")
	}

	b, err := json.Marshal(def)
	if err != nil {
		t.Fatalf("failed to marshal toolDef: %v", err)
	}
	if strings.Contains(string(b), "requires_approval") {
		t.Errorf("serialized toolDef contains 'requires_approval', which is not an OpenAI concept: %s", b)
	}
}

// --- convertMessages / convertMessage edge cases ---

func TestConvertMessages_EmptySlice(t *testing.T) {
	t.Parallel()
	got, err := convertMessages([]llm.Message{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len(got) = %d, want 0", len(got))
	}
}

func TestConvertMessage_UnsupportedRole(t *testing.T) {
	t.Parallel()
	msg := llm.Message{
		Role:    "custom",
		Content: []llm.ContentBlock{llm.TextBlock{Type: "text", Text: "hello"}},
	}

	got, err := convertMessage(msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Role != "custom" {
		t.Errorf("Role = %q, want %q", got.Role, "custom")
	}
	str, ok := got.Content.(string)
	if !ok {
		t.Fatalf("Content type = %T, want string", got.Content)
	}
	if str != "hello" {
		t.Errorf("Content = %q, want %q", str, "hello")
	}
}

// --- convertMessage routing ---

func TestConvertMessage_RoutesByRole(t *testing.T) {
	// Tool role → convertToolResult
	toolMsg := llm.Message{
		Role: llm.RoleTool,
		Content: []llm.ContentBlock{
			llm.ToolResultBlock{Type: "tool_result", ToolUseID: "t1", Content: "ok"},
		},
	}
	cm, err := convertMessage(toolMsg)
	if err != nil {
		t.Fatalf("tool role: unexpected error: %v", err)
	}
	if cm.Role != "tool" || cm.ToolCallID != "t1" {
		t.Errorf("tool routing: got Role=%q ToolCallID=%q", cm.Role, cm.ToolCallID)
	}

	// Assistant role → convertAssistant
	asstMsg := llm.Message{
		Role:    llm.RoleAssistant,
		Content: []llm.ContentBlock{llm.TextBlock{Type: "text", Text: "hi"}},
	}
	cm, err = convertMessage(asstMsg)
	if err != nil {
		t.Fatalf("assistant role: unexpected error: %v", err)
	}
	if cm.Role != "assistant" {
		t.Errorf("assistant routing: got Role=%q", cm.Role)
	}

	// User role → convertStandardMessage
	userMsg := llm.Message{
		Role:    llm.RoleUser,
		Content: []llm.ContentBlock{llm.TextBlock{Type: "text", Text: "hey"}},
	}
	cm, err = convertMessage(userMsg)
	if err != nil {
		t.Fatalf("user role: unexpected error: %v", err)
	}
	if cm.Role != "user" {
		t.Errorf("user routing: got Role=%q", cm.Role)
	}

	// System role → convertStandardMessage
	sysMsg := llm.Message{
		Role:    llm.RoleSystem,
		Content: []llm.ContentBlock{llm.TextBlock{Type: "text", Text: "sys"}},
	}
	cm, err = convertMessage(sysMsg)
	if err != nil {
		t.Fatalf("system role: unexpected error: %v", err)
	}
	if cm.Role != "system" {
		t.Errorf("system routing: got Role=%q", cm.Role)
	}
}

// --- parseStreamPayload ---

func TestParseStreamPayload_TextDelta(t *testing.T) {
	t.Parallel()
	payload := `{"id":"1","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}`
	chunks := parseStreamPayload(payload)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	td, ok := chunks[0].(llm.TextDeltaChunk)
	if !ok {
		t.Fatalf("expected TextDeltaChunk, got %T", chunks[0])
	}
	if td.Text != "Hello" {
		t.Errorf("Text = %q, want %q", td.Text, "Hello")
	}
}

func TestParseStreamPayload_ToolCallStart(t *testing.T) {
	t.Parallel()
	payload := `{"id":"1","model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_abc","type":"function","function":{"name":"get_weather","arguments":""}}]},"finish_reason":null}]}`
	chunks := parseStreamPayload(payload)
	if len(chunks) < 1 {
		t.Fatal("expected at least 1 chunk")
	}
	tcs, ok := chunks[0].(llm.ToolCallStartChunk)
	if !ok {
		t.Fatalf("expected ToolCallStartChunk, got %T", chunks[0])
	}
	if tcs.ID != "call_abc" {
		t.Errorf("ID = %q, want %q", tcs.ID, "call_abc")
	}
	if tcs.Name != "get_weather" {
		t.Errorf("Name = %q, want %q", tcs.Name, "get_weather")
	}
	if tcs.Index != 0 {
		t.Errorf("Index = %d, want 0", tcs.Index)
	}
}

func TestParseStreamPayload_ToolCallArgs(t *testing.T) {
	t.Parallel()
	payload := `{"id":"1","model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"ci"}}]},"finish_reason":null}]}`
	chunks := parseStreamPayload(payload)
	if len(chunks) < 1 {
		t.Fatal("expected at least 1 chunk")
	}
	tca, ok := chunks[0].(llm.ToolCallArgsChunk)
	if !ok {
		t.Fatalf("expected ToolCallArgsChunk, got %T", chunks[0])
	}
	if tca.Delta != `{"ci` {
		t.Errorf("Delta = %q, want %q", tca.Delta, `{"ci`)
	}
}

func TestParseStreamPayload_Done(t *testing.T) {
	t.Parallel()
	payload := `{"id":"1","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`
	chunks := parseStreamPayload(payload)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	done, ok := chunks[0].(llm.DoneChunk)
	if !ok {
		t.Fatalf("expected DoneChunk, got %T", chunks[0])
	}
	if done.FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want %q", done.FinishReason, "stop")
	}
}

func TestParseStreamPayload_SkipsRoleOnly(t *testing.T) {
	t.Parallel()
	payload := `{"id":"1","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`
	chunks := parseStreamPayload(payload)
	if len(chunks) != 0 {
		t.Errorf("expected 0 chunks for role-only delta, got %d", len(chunks))
	}
}

func TestParseStreamPayload_ContentAndDone(t *testing.T) {
	t.Parallel()
	payload := `{"id":"1","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"bye"},"finish_reason":"stop"}]}`
	chunks := parseStreamPayload(payload)
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(chunks))
	}
	if _, ok := chunks[0].(llm.TextDeltaChunk); !ok {
		t.Fatalf("chunk[0]: expected TextDeltaChunk, got %T", chunks[0])
	}
	if _, ok := chunks[1].(llm.DoneChunk); !ok {
		t.Fatalf("chunk[1]: expected DoneChunk, got %T", chunks[1])
	}
}

func TestParseStreamPayload_FinishReasonWithUsage_YieldsOneDoneChunk(t *testing.T) {
	t.Parallel()
	payload := `{"id":"1","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`
	chunks := parseStreamPayload(payload)

	var done []llm.DoneChunk
	for _, c := range chunks {
		if d, ok := c.(llm.DoneChunk); ok {
			done = append(done, d)
		}
	}
	if len(done) != 1 {
		t.Fatalf("expected exactly 1 DoneChunk, got %d (total chunks: %d)", len(done), len(chunks))
	}
	if done[0].FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want %q", done[0].FinishReason, "stop")
	}
	if done[0].Usage == nil || done[0].Usage.InputTokens != 10 || done[0].Usage.OutputTokens != 5 {
		t.Errorf("Usage = %+v, want input=10 output=5", done[0].Usage)
	}
}

func TestParseStreamPayload_UsageOnlyFrame_YieldsUsageOnlyDoneChunk(t *testing.T) {
	t.Parallel()
	payload := `{"id":"1","model":"gpt-4o","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`
	chunks := parseStreamPayload(payload)
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
	if done.Usage == nil || done.Usage.InputTokens != 7 || done.Usage.OutputTokens != 3 {
		t.Errorf("Usage = %+v, want input=7 output=3", done.Usage)
	}
}

func TestParseStreamPayload_InvalidJSON(t *testing.T) {
	t.Parallel()
	chunks := parseStreamPayload("not json")
	if len(chunks) != 0 {
		t.Errorf("expected 0 chunks for invalid JSON, got %d", len(chunks))
	}
}

// --- ReasoningBlock tests ---

func TestConvertResponseMessage_ReasoningBlock(t *testing.T) {
	t.Parallel()
	rm := respMessage{
		Role:             "assistant",
		Content:          strPtr("final answer"),
		ReasoningContent: strPtr("step-by-step thinking"),
	}

	got, err := convertResponseMessage(rm)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Content) != 2 {
		t.Fatalf("len(Content) = %d, want 2", len(got.Content))
	}

	rb, ok := got.Content[0].(llm.ReasoningBlock)
	if !ok {
		t.Fatalf("Content[0] type = %T, want ReasoningBlock", got.Content[0])
	}
	if rb.Type != "reasoning" {
		t.Errorf("ReasoningBlock.Type = %q, want %q", rb.Type, "reasoning")
	}
	if rb.Content != "step-by-step thinking" {
		t.Errorf("ReasoningBlock.Content = %q, want %q", rb.Content, "step-by-step thinking")
	}

	tb, ok := got.Content[1].(llm.TextBlock)
	if !ok {
		t.Fatalf("Content[1] type = %T, want TextBlock", got.Content[1])
	}
	if tb.Text != "final answer" {
		t.Errorf("TextBlock.Text = %q, want %q", tb.Text, "final answer")
	}
}

func TestConvertResponseMessage_NoReasoningBlock(t *testing.T) {
	t.Parallel()
	rm := respMessage{
		Role:    "assistant",
		Content: strPtr("no reasoning here"),
	}

	got, err := convertResponseMessage(rm)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Content) != 1 {
		t.Fatalf("len(Content) = %d, want 1", len(got.Content))
	}

	if _, ok := got.Content[0].(llm.TextBlock); !ok {
		t.Fatalf("Content[0] type = %T, want TextBlock", got.Content[0])
	}
}

func TestConvertResponseMessage_EmptyReasoningContent(t *testing.T) {
	t.Parallel()
	rm := respMessage{
		Role:             "assistant",
		Content:          strPtr("text only"),
		ReasoningContent: strPtr(""),
	}

	got, err := convertResponseMessage(rm)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Content) != 1 {
		t.Fatalf("len(Content) = %d, want 1 (empty reasoning should be skipped)", len(got.Content))
	}

	if _, ok := got.Content[0].(llm.TextBlock); !ok {
		t.Fatalf("Content[0] type = %T, want TextBlock", got.Content[0])
	}
}

func TestConvertResponseMessage_ReasoningWithToolCalls(t *testing.T) {
	t.Parallel()
	rm := respMessage{
		Role:             "assistant",
		ReasoningContent: strPtr("planning tool use"),
		ToolCalls: []toolCall{
			{ID: "tc_r1", Type: "function", Function: functionCall{Name: "search", Arguments: `{"q":"test"}`}},
		},
	}

	got, err := convertResponseMessage(rm)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Content) != 2 {
		t.Fatalf("len(Content) = %d, want 2", len(got.Content))
	}

	rb, ok := got.Content[0].(llm.ReasoningBlock)
	if !ok {
		t.Fatalf("Content[0] type = %T, want ReasoningBlock", got.Content[0])
	}
	if rb.Content != "planning tool use" {
		t.Errorf("ReasoningBlock.Content = %q, want %q", rb.Content, "planning tool use")
	}

	tub, ok := got.Content[1].(llm.ToolUseBlock)
	if !ok {
		t.Fatalf("Content[1] type = %T, want ToolUseBlock", got.Content[1])
	}
	if tub.Name != "search" {
		t.Errorf("ToolUseBlock.Name = %q, want %q", tub.Name, "search")
	}
}

func TestConvertResponseMessage_ReasoningOnly(t *testing.T) {
	t.Parallel()
	rm := respMessage{
		Role:             "assistant",
		ReasoningContent: strPtr("only thinking, no text"),
	}

	got, err := convertResponseMessage(rm)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Content) != 1 {
		t.Fatalf("len(Content) = %d, want 1", len(got.Content))
	}

	rb, ok := got.Content[0].(llm.ReasoningBlock)
	if !ok {
		t.Fatalf("Content[0] type = %T, want ReasoningBlock", got.Content[0])
	}
	if rb.Content != "only thinking, no text" {
		t.Errorf("ReasoningBlock.Content = %q, want %q", rb.Content, "only thinking, no text")
	}
}

func TestConvertAssistant_ReasoningBlockStripped(t *testing.T) {
	t.Parallel()
	msg := llm.Message{
		Role: llm.RoleAssistant,
		Content: []llm.ContentBlock{
			llm.ReasoningBlock{Type: "reasoning", Content: "internal thought"},
			llm.TextBlock{Type: "text", Text: "public answer"},
		},
	}

	got, err := convertAssistant(msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	str, ok := got.Content.(string)
	if !ok {
		t.Fatalf("Content type = %T, want string", got.Content)
	}
	if str != "public answer" {
		t.Errorf("Content = %q, want %q", str, "public answer")
	}
	if len(got.ToolCalls) != 0 {
		t.Errorf("ToolCalls len = %d, want 0", len(got.ToolCalls))
	}
	if got.ReasoningContent == nil {
		t.Fatal("ReasoningContent is nil, want non-nil")
	}
	if *got.ReasoningContent != "internal thought" {
		t.Errorf("ReasoningContent = %q, want %q", *got.ReasoningContent, "internal thought")
	}
}

func TestConvertStandard_ReasoningBlockStripped(t *testing.T) {
	t.Parallel()
	msg := llm.Message{
		Role: llm.RoleUser,
		Content: []llm.ContentBlock{
			llm.ReasoningBlock{Type: "reasoning", Content: "should be dropped"},
			llm.TextBlock{Type: "text", Text: "actual user text"},
		},
	}

	got, err := convertStandardMessage(msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	parts, ok := got.Content.([]contentPart)
	if !ok {
		t.Fatalf("Content type = %T, want []contentPart", got.Content)
	}
	if len(parts) != 1 {
		t.Fatalf("len(parts) = %d, want 1 (ReasoningBlock should be stripped)", len(parts))
	}
	if parts[0].Text != "actual user text" {
		t.Errorf("parts[0].Text = %q, want %q", parts[0].Text, "actual user text")
	}
}

func TestConvertAssistant_ReasoningContentJSON(t *testing.T) {
	t.Parallel()
	msg := llm.Message{
		Role: llm.RoleAssistant,
		Content: []llm.ContentBlock{
			llm.ReasoningBlock{Type: "reasoning", Content: "step 1: analyze"},
			llm.TextBlock{Type: "text", Text: "answer"},
		},
	}

	got, err := convertAssistant(msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	rc, ok := parsed["reasoning_content"]
	if !ok {
		t.Fatal("JSON output missing 'reasoning_content' field")
	}
	rcStr, ok := rc.(string)
	if !ok {
		t.Fatalf("reasoning_content type = %T, want string", rc)
	}
	if rcStr != "step 1: analyze" {
		t.Errorf("reasoning_content = %q, want %q", rcStr, "step 1: analyze")
	}

	content, _ := parsed["content"].(string)
	if content != "answer" {
		t.Errorf("content = %q, want %q", content, "answer")
	}
}

func TestConvertAssistant_NoReasoningBlock_NilReasoningContent(t *testing.T) {
	t.Parallel()
	msg := llm.Message{
		Role:    llm.RoleAssistant,
		Content: []llm.ContentBlock{llm.TextBlock{Type: "text", Text: "plain"}},
	}

	got, err := convertAssistant(msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got.ReasoningContent != nil {
		t.Errorf("ReasoningContent = %v, want nil when no ReasoningBlock present", got.ReasoningContent)
	}

	data, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), "reasoning_content") {
		t.Errorf("serialized JSON should omit reasoning_content when nil, got: %s", data)
	}
}
