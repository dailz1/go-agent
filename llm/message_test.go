package llm

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/tool"
)

// --- Constructor tests ---

func TestSystemMessage(t *testing.T) {
	msg := SystemMessage("you are a helper")
	if msg.Role != RoleSystem {
		t.Errorf("expected Role %q, got %q", RoleSystem, msg.Role)
	}
	if len(msg.Content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(msg.Content))
	}
	tb, ok := msg.Content[0].(TextBlock)
	if !ok {
		t.Fatalf("expected TextBlock, got %T", msg.Content[0])
	}
	if tb.Text != "you are a helper" {
		t.Errorf("expected text %q, got %q", "you are a helper", tb.Text)
	}
}

func TestUserMessage(t *testing.T) {
	msg := UserMessage("hello")
	if msg.Role != RoleUser {
		t.Errorf("expected Role %q, got %q", RoleUser, msg.Role)
	}
	if len(msg.Content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(msg.Content))
	}
	tb, ok := msg.Content[0].(TextBlock)
	if !ok {
		t.Fatalf("expected TextBlock, got %T", msg.Content[0])
	}
	if tb.Text != "hello" {
		t.Errorf("expected text %q, got %q", "hello", tb.Text)
	}
}

func TestAssistantMessage(t *testing.T) {
	msg := AssistantMessage("hi there")
	if msg.Role != RoleAssistant {
		t.Errorf("expected Role %q, got %q", RoleAssistant, msg.Role)
	}
	if len(msg.Content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(msg.Content))
	}
	tb, ok := msg.Content[0].(TextBlock)
	if !ok {
		t.Fatalf("expected TextBlock, got %T", msg.Content[0])
	}
	if tb.Text != "hi there" {
		t.Errorf("expected text %q, got %q", "hi there", tb.Text)
	}
}

func TestAssistantToolCallMessage(t *testing.T) {
	calls := []ToolUseBlock{
		{ID: "call_1", Name: "search", Input: json.RawMessage(`{"q":"go"}`)},
		{ID: "call_2", Name: "read", Input: json.RawMessage(`{"path":"/tmp"}`)},
	}
	msg := AssistantToolCallMessage(calls...)
	if msg.Role != RoleAssistant {
		t.Errorf("expected Role %q, got %q", RoleAssistant, msg.Role)
	}
	if len(msg.Content) != 2 {
		t.Fatalf("expected 2 content blocks, got %d", len(msg.Content))
	}
	for i, block := range msg.Content {
		tu, ok := block.(ToolUseBlock)
		if !ok {
			t.Fatalf("block[%d]: expected ToolUseBlock, got %T", i, block)
		}
		if tu.Type != "tool_use" {
			t.Errorf("block[%d]: expected type %q, got %q", i, "tool_use", tu.Type)
		}
	}
}

func TestToolResultMessage(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		msg := ToolResultMessage("call_abc", tool.NewTextResult("ok"))
		if msg.Role != RoleTool {
			t.Errorf("expected Role %q, got %q", RoleTool, msg.Role)
		}
		if len(msg.Content) != 1 {
			t.Fatalf("expected 1 content block, got %d", len(msg.Content))
		}
		tr, ok := msg.Content[0].(ToolResultBlock)
		if !ok {
			t.Fatalf("expected ToolResultBlock, got %T", msg.Content[0])
		}
		if tr.Content != "ok" {
			t.Errorf("expected content %q, got %q", "ok", tr.Content)
		}
		if tr.IsError {
			t.Error("expected IsError=false for success result")
		}
		if tr.ToolUseID != "call_abc" {
			t.Errorf("expected ToolUseID %q, got %q", "call_abc", tr.ToolUseID)
		}
	})

	t.Run("error", func(t *testing.T) {
		msg := ToolResultMessage("call_err", tool.NewErrorResult("bad"))
		tr := msg.Content[0].(ToolResultBlock)
		if !tr.IsError {
			t.Error("expected IsError=true for error result")
		}
		if tr.Content != "bad" {
			t.Errorf("expected content %q, got %q", "bad", tr.Content)
		}
	})
}

// --- JSON round-trip tests ---

func TestMessageJSONRoundTrip_SystemText(t *testing.T) {
	orig := SystemMessage("you are helpful")
	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got Message
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.Role != orig.Role {
		t.Errorf("role: expected %q, got %q", orig.Role, got.Role)
	}
	if len(got.Content) != 1 {
		t.Fatalf("content blocks: expected 1, got %d", len(got.Content))
	}
	tb, ok := got.Content[0].(TextBlock)
	if !ok {
		t.Fatalf("expected TextBlock, got %T", got.Content[0])
	}
	if tb.Text != "you are helpful" {
		t.Errorf("text: expected %q, got %q", "you are helpful", tb.Text)
	}
}

func TestMessageJSONRoundTrip_UserMultipleBlocks(t *testing.T) {
	orig := Message{
		Role: RoleUser,
		Content: []ContentBlock{
			TextBlock{Type: "text", Text: "describe this"},
			ImageBlock{Type: "image", URL: "https://example.com/img.png"},
		},
	}
	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got Message
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(got.Content) != 2 {
		t.Fatalf("expected 2 content blocks, got %d", len(got.Content))
	}
	tb, ok := got.Content[0].(TextBlock)
	if !ok || tb.Text != "describe this" {
		t.Errorf("block[0]: expected TextBlock with text %q", "describe this")
	}
	ib, ok := got.Content[1].(ImageBlock)
	if !ok || ib.URL != "https://example.com/img.png" {
		t.Errorf("block[1]: expected ImageBlock with URL %q", "https://example.com/img.png")
	}
}

func TestMessageJSONRoundTrip_AssistantToolUse(t *testing.T) {
	input := json.RawMessage(`{"city":"Berlin","unit":"celsius"}`)
	orig := Message{
		Role: RoleAssistant,
		Content: []ContentBlock{
			ToolUseBlock{Type: "tool_use", ID: "call_42", Name: "get_weather", Input: input},
		},
	}
	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got Message
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(got.Content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(got.Content))
	}
	tu, ok := got.Content[0].(ToolUseBlock)
	if !ok {
		t.Fatalf("expected ToolUseBlock, got %T", got.Content[0])
	}
	if tu.ID != "call_42" || tu.Name != "get_weather" {
		t.Errorf("got ID=%q Name=%q, want call_42 / get_weather", tu.ID, tu.Name)
	}
	// Input must be preserved as json.RawMessage
	var expectedInput, gotInput map[string]string
	if err := json.Unmarshal(input, &expectedInput); err != nil {
		t.Fatalf("unmarshal expected input: %v", err)
	}
	if err := json.Unmarshal(tu.Input, &gotInput); err != nil {
		t.Fatalf("unmarshal got input: %v", err)
	}
	for k, v := range expectedInput {
		if gotInput[k] != v {
			t.Errorf("input[%q]: expected %q, got %q", k, v, gotInput[k])
		}
	}
}

func TestMessageJSONRoundTrip_ToolResult(t *testing.T) {
	orig := ToolResultMessage("call_xyz", tool.NewTextResult("temperature is 22°C"))
	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got Message
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.Role != RoleTool {
		t.Errorf("role: expected %q, got %q", RoleTool, got.Role)
	}
	if len(got.Content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(got.Content))
	}
	tr, ok := got.Content[0].(ToolResultBlock)
	if !ok {
		t.Fatalf("expected ToolResultBlock, got %T", got.Content[0])
	}
	if tr.ToolUseID != "call_xyz" {
		t.Errorf("tool_use_id: expected %q, got %q", "call_xyz", tr.ToolUseID)
	}
	if tr.Content != "temperature is 22°C" {
		t.Errorf("content: expected %q, got %q", "temperature is 22°C", tr.Content)
	}
	if tr.IsError {
		t.Error("is_error: expected false")
	}
}

// --- Edge case tests ---

func TestMessageUnmarshal_UnknownBlockType(t *testing.T) {
	raw := `{"role":"user","content":[{"type":"unknown","foo":"bar"}]}`
	var msg Message
	err := json.Unmarshal([]byte(raw), &msg)
	if err == nil {
		t.Fatal("expected error for unknown block type, got nil")
	}
}

func TestMessageUnmarshal_EmptyContent(t *testing.T) {
	raw := `{"role":"user","content":[]}`
	var msg Message
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if msg.Role != RoleUser {
		t.Errorf("role: expected %q, got %q", RoleUser, msg.Role)
	}
	if len(msg.Content) != 0 {
		t.Errorf("expected empty content, got %d blocks", len(msg.Content))
	}
	if msg.Content == nil {
		t.Error("Content should be empty slice, not nil")
	}
}

func TestMessageMarshal_TextBlockSerializesType(t *testing.T) {
	tb := TextBlock{Type: "text", Text: "hello"}
	data, err := json.Marshal(tb)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}
	if m["type"] != "text" {
		t.Errorf("expected type %q, got %q", "text", m["type"])
	}
	if m["text"] != "hello" {
		t.Errorf("expected text %q, got %q", "hello", m["text"])
	}
}

func TestMessageUnmarshal_ContentIsString(t *testing.T) {
	t.Parallel()
	raw := `{"role":"user","content":"hello"}`
	var msg Message
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if msg.Role != RoleUser {
		t.Errorf("role: expected %q, got %q", RoleUser, msg.Role)
	}
	if len(msg.Content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(msg.Content))
	}
	tb, ok := msg.Content[0].(TextBlock)
	if !ok {
		t.Fatalf("expected TextBlock, got %T", msg.Content[0])
	}
	if tb.Type != "text" {
		t.Errorf("type: expected %q, got %q", "text", tb.Type)
	}
	if tb.Text != "hello" {
		t.Errorf("text: expected %q, got %q", "hello", tb.Text)
	}
}

func TestMessageUnmarshal_ContentIsNull(t *testing.T) {
	t.Parallel()
	raw := `{"role":"user","content":null}`
	var msg Message
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if msg.Role != RoleUser {
		t.Errorf("role: expected %q, got %q", RoleUser, msg.Role)
	}
	if len(msg.Content) != 0 {
		t.Errorf("expected empty content, got %d blocks", len(msg.Content))
	}
	if msg.Content == nil {
		t.Error("Content should be empty slice, not nil")
	}
}

func TestMessageUnmarshal_ContentIsNumber(t *testing.T) {
	t.Parallel()
	raw := `{"role":"user","content":42}`
	var msg Message
	err := json.Unmarshal([]byte(raw), &msg)
	if err == nil {
		t.Fatal("expected error for numeric content, got nil")
	}
}

func TestMessageUnmarshal_EmptyStringContent(t *testing.T) {
	t.Parallel()
	raw := `{"role":"assistant","content":""}`
	var msg Message
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if msg.Role != RoleAssistant {
		t.Errorf("role: expected %q, got %q", RoleAssistant, msg.Role)
	}
	if len(msg.Content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(msg.Content))
	}
	tb, ok := msg.Content[0].(TextBlock)
	if !ok {
		t.Fatalf("expected TextBlock, got %T", msg.Content[0])
	}
	if tb.Text != "" {
		t.Errorf("text: expected empty string, got %q", tb.Text)
	}
}

func TestMessageUnmarshal_InvalidJSON(t *testing.T) {
	t.Parallel()
	raw := `{not valid json}`
	var msg Message
	err := json.Unmarshal([]byte(raw), &msg)
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

func TestMessageUnmarshal_EmptyToolUseBlockInput(t *testing.T) {
	t.Parallel()
	orig := Message{
		Role: RoleAssistant,
		Content: []ContentBlock{
			ToolUseBlock{Type: "tool_use", ID: "call_empty", Name: "ping", Input: json.RawMessage(`""`)},
		},
	}
	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Message
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	tu, ok := got.Content[0].(ToolUseBlock)
	if !ok {
		t.Fatalf("expected ToolUseBlock, got %T", got.Content[0])
	}
	if tu.ID != "call_empty" || tu.Name != "ping" {
		t.Errorf("got ID=%q Name=%q, want call_empty / ping", tu.ID, tu.Name)
	}
	if string(tu.Input) != `""` {
		t.Errorf("input: expected %q, got %q", `""`, string(tu.Input))
	}
}

func TestMessageUnmarshal_LargeContent(t *testing.T) {
	t.Parallel()
	largeText := strings.Repeat("x", 1<<20)
	msg := UserMessage(largeText)
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Message
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	tb, ok := got.Content[0].(TextBlock)
	if !ok {
		t.Fatalf("expected TextBlock, got %T", got.Content[0])
	}
	if len(tb.Text) != len(largeText) {
		t.Errorf("text length: expected %d, got %d", len(largeText), len(tb.Text))
	}
}

func TestMessageUnmarshal_LargeContentString(t *testing.T) {
	t.Parallel()
	largeText := strings.Repeat("a", 100_000)
	raw := fmt.Sprintf(`{"role":"user","content":%q}`, largeText)
	var msg Message
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	tb, ok := msg.Content[0].(TextBlock)
	if !ok {
		t.Fatalf("expected TextBlock, got %T", msg.Content[0])
	}
	if len(tb.Text) != len(largeText) {
		t.Errorf("text length: expected %d, got %d", len(largeText), len(tb.Text))
	}
}

func TestContentBlockSealedInterface(t *testing.T) {
	t.Parallel()
	var _ ContentBlock = TextBlock{}
	var _ ContentBlock = (*TextBlock)(nil)
	var _ ContentBlock = ImageBlock{}
	var _ ContentBlock = (*ImageBlock)(nil)
	var _ ContentBlock = ToolUseBlock{}
	var _ ContentBlock = (*ToolUseBlock)(nil)
	var _ ContentBlock = ToolResultBlock{}
	var _ ContentBlock = (*ToolResultBlock)(nil)
}

// --- ReasoningBlock tests ---

func TestReasoningBlock_BlockType(t *testing.T) {
	t.Parallel()
	rb := ReasoningBlock{Type: "reasoning", Content: "thinking..."}
	if rb.blockType() != "reasoning" {
		t.Errorf("blockType: expected %q, got %q", "reasoning", rb.blockType())
	}
}

func TestReasoningBlock_Serialize(t *testing.T) {
	t.Parallel()
	rb := ReasoningBlock{Type: "reasoning", Content: "I should consider..."}
	data, err := json.Marshal(rb)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}
	if m["type"] != "reasoning" {
		t.Errorf("type: expected %q, got %q", "reasoning", m["type"])
	}
	if m["content"] != "I should consider..." {
		t.Errorf("content: expected %q, got %q", "I should consider...", m["content"])
	}
}

func TestReasoningBlock_Deserialize(t *testing.T) {
	t.Parallel()
	raw := `{"type":"reasoning","content":"step by step"}`
	b, err := unmarshalContentBlock([]byte(raw))
	if err != nil {
		t.Fatalf("unmarshalContentBlock: %v", err)
	}
	rb, ok := b.(ReasoningBlock)
	if !ok {
		t.Fatalf("expected ReasoningBlock, got %T", b)
	}
	if rb.Type != "reasoning" {
		t.Errorf("type: expected %q, got %q", "reasoning", rb.Type)
	}
	if rb.Content != "step by step" {
		t.Errorf("content: expected %q, got %q", "step by step", rb.Content)
	}
}

func TestReasoningBlock_InMessage(t *testing.T) {
	t.Parallel()
	msg := Message{
		Role: RoleAssistant,
		Content: []ContentBlock{
			ReasoningBlock{Type: "reasoning", Content: "let me think"},
			TextBlock{Type: "text", Text: "Here is the answer"},
		},
	}
	if len(msg.Content) != 2 {
		t.Fatalf("expected 2 content blocks, got %d", len(msg.Content))
	}
	rb, ok := msg.Content[0].(ReasoningBlock)
	if !ok {
		t.Fatalf("block[0]: expected ReasoningBlock, got %T", msg.Content[0])
	}
	if rb.Content != "let me think" {
		t.Errorf("block[0] content: expected %q, got %q", "let me think", rb.Content)
	}
	tb, ok := msg.Content[1].(TextBlock)
	if !ok || tb.Text != "Here is the answer" {
		t.Errorf("block[1]: expected TextBlock with text %q", "Here is the answer")
	}
}

func TestReasoningBlock_SealedInterface(t *testing.T) {
	t.Parallel()
	var _ ContentBlock = ReasoningBlock{}
	var _ ContentBlock = (*ReasoningBlock)(nil)
}

func TestMessageJSONRoundTrip_ReasoningBlock(t *testing.T) {
	t.Parallel()
	orig := Message{
		Role: RoleAssistant,
		Content: []ContentBlock{
			ReasoningBlock{Type: "reasoning", Content: "analyzing the query"},
			TextBlock{Type: "text", Text: "The answer is 42"},
		},
	}
	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got Message
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.Role != RoleAssistant {
		t.Errorf("role: expected %q, got %q", RoleAssistant, got.Role)
	}
	if len(got.Content) != 2 {
		t.Fatalf("content blocks: expected 2, got %d", len(got.Content))
	}
	rb, ok := got.Content[0].(ReasoningBlock)
	if !ok {
		t.Fatalf("block[0]: expected ReasoningBlock, got %T", got.Content[0])
	}
	if rb.Content != "analyzing the query" {
		t.Errorf("block[0] content: expected %q, got %q", "analyzing the query", rb.Content)
	}
	tb, ok := got.Content[1].(TextBlock)
	if !ok {
		t.Fatalf("block[1]: expected TextBlock, got %T", got.Content[1])
	}
	if tb.Text != "The answer is 42" {
		t.Errorf("block[1] text: expected %q, got %q", "The answer is 42", tb.Text)
	}
}
