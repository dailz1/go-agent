package llm

import (
	"encoding/json"
	"fmt"
	"testing"
)

// 契约钉子（issue #24）：密封数据接口只持指针形态——解码产指针，
// 具体类型 T 与 *T 编码出相同 JSON 字节，typed-nil 仍满足接口。

func TestUnmarshalContentBlockEmitsPointers(t *testing.T) {
	tests := []struct {
		name string
		json string
		want string
	}{
		{"text", `{"type":"text","text":"hi"}`, "*llm.TextBlock"},
		{"reasoning", `{"type":"reasoning","content":"why"}`, "*llm.ReasoningBlock"},
		{"reasoning_item", `{"type":"reasoning_item","id":"rs_1"}`, "*llm.ReasoningItemBlock"},
		{"image", `{"type":"image","url":"https://example.com/i.png"}`, "*llm.ImageBlock"},
		{"tool_use", `{"type":"tool_use","id":"c1","name":"echo","input":{}}`, "*llm.ToolUseBlock"},
		{"tool_result", `{"type":"tool_result","tool_use_id":"c1","content":"done"}`, "*llm.ToolResultBlock"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, err := unmarshalContentBlock([]byte(tc.json))
			if err != nil {
				t.Fatalf("unmarshalContentBlock: %v", err)
			}
			if got := fmt.Sprintf("%T", b); got != tc.want {
				t.Fatalf("dynamic type = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestValueAndPointerEncodeIdenticalBytes 钉住日志兼容性结论：同一块以
// T 与 *T 承载时编码出的 JSON 字节相同，翻转前的录制与线程日志在翻转后
// 依然字节兼容（issue #24，DESIGN.md 录制 grammar）。
func TestValueAndPointerEncodeIdenticalBytes(t *testing.T) {
	textV, textP := TextBlock{Type: "text", Text: "hi"}, &TextBlock{Type: "text", Text: "hi"}
	useV, useP := ToolUseBlock{Type: "tool_use", ID: "c1", Name: "echo", Input: json.RawMessage(`{"x":1}`)}, &ToolUseBlock{Type: "tool_use", ID: "c1", Name: "echo", Input: json.RawMessage(`{"x":1}`)}

	vb, err := json.Marshal(textV)
	if err != nil {
		t.Fatalf("marshal value: %v", err)
	}
	pb, err := json.Marshal(textP)
	if err != nil {
		t.Fatalf("marshal pointer: %v", err)
	}
	if string(vb) != string(pb) {
		t.Fatalf("TextBlock bytes differ: value=%s pointer=%s", vb, pb)
	}

	vb, err = json.Marshal(useV)
	if err != nil {
		t.Fatalf("marshal value: %v", err)
	}
	pb, err = json.Marshal(useP)
	if err != nil {
		t.Fatalf("marshal pointer: %v", err)
	}
	if string(vb) != string(pb) {
		t.Fatalf("ToolUseBlock bytes differ: value=%s pointer=%s", vb, pb)
	}
}

// TestMessageMarshalUnmarshalIdempotent 钉住指针形态消息的 JSON 往返
// 字节稳定：解码重建指针形态，再编码结果与首次编码一致。
func TestMessageMarshalUnmarshalIdempotent(t *testing.T) {
	msg := Message{
		Role: RoleUser,
		Content: []ContentBlock{
			&TextBlock{Type: "text", Text: "hello"},
			&ToolResultBlock{Type: "tool_result", ToolUseID: "c1", Content: "done"},
		},
	}
	first, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Message
	if err := json.Unmarshal(first, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	second, err := json.Marshal(back)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if string(first) != string(second) {
		t.Fatalf("round trip not byte-stable:\n first=%s\nsecond=%s", first, second)
	}
	for i, block := range back.Content {
		if isNilBlock(block) {
			t.Fatalf("block %d decoded as nil", i)
		}
	}
}

func isNilBlock(b ContentBlock) bool {
	switch v := b.(type) {
	case *TextBlock:
		return v == nil
	case *ReasoningBlock:
		return v == nil
	case *ImageBlock:
		return v == nil
	case *ToolUseBlock:
		return v == nil
	case *ReasoningItemBlock:
		return v == nil
	case *ToolResultBlock:
		return v == nil
	default:
		return false
	}
}

// TestTypedNilStillSatisfiesSealedInterfaces 钉住 typed-nil 仍可表示：
// nil 守卫依然是承重逻辑，不因收紧而消失。
func TestTypedNilStillSatisfiesSealedInterfaces(t *testing.T) {
	var b ContentBlock = (*TextBlock)(nil)
	if _, ok := b.(*TextBlock); !ok || b == nil {
		t.Fatalf("typed-nil ContentBlock: %#v", b)
	}
	var c Chunk = (*DoneChunk)(nil)
	if _, ok := c.(*DoneChunk); !ok || c == nil {
		t.Fatalf("typed-nil Chunk: %#v", c)
	}
}
