package agent

import (
	"testing"

	"github.com/dailz1/go-agent/pkg/llm"
	"github.com/dailz1/go-agent/pkg/tool"
)

// TestStreamingReasoningItemOrderPreserved pins the H1 invariant: when the
// Responses protocol interleaves reasoning items and function calls
// (r0, c1, r2, c3), the assembled assistant message preserves exactly that
// declaration order — the encrypted reasoning stays associated with the
// tool call it precedes instead of being globally hoisted.
func TestStreamingReasoningItemOrderPreserved(t *testing.T) {
	r0 := llm.ReasoningItemBlock{Type: "reasoning_item", ID: "rs_0", EncryptedContent: "enc-0", Summary: []string{"s0"}}
	r1 := llm.ReasoningItemBlock{Type: "reasoning_item", ID: "rs_1", EncryptedContent: "enc-1", Summary: []string{"s1"}}

	provider := NewMockStreamingProvider([][]llm.Chunk{
		{
			llm.ReasoningItemChunk{OutputIndex: 0, Item: r0},
			llm.ToolCallStartChunk{Index: 1, ID: "call_1", Name: "echo"},
			llm.ToolCallArgsChunk{Index: 1, Delta: `{}`},
			llm.ReasoningItemChunk{OutputIndex: 2, Item: r1},
			llm.ToolCallStartChunk{Index: 3, ID: "call_2", Name: "echo"},
			llm.ToolCallArgsChunk{Index: 3, Delta: `{}`},
			llm.DoneChunk{FinishReason: "tool_calls"},
		},
		{
			llm.TextDeltaChunk{Text: "done"},
			llm.DoneChunk{FinishReason: "stop"},
		},
	})
	a := New(provider, newEchoRegistry(), WithLogger(discardLogger()))
	res, err := a.Run(t.Context(), "interleave")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// First-round assistant message: the four blocks in declaration order.
	first := res.History[1] // system, user, assistant
	if first.Role != llm.RoleAssistant || len(first.Content) != 4 {
		t.Fatalf("first assistant message = %+v (%d blocks), want 4 blocks", first, len(first.Content))
	}
	wantIDs := []string{"rs_0", "call_1", "rs_1", "call_2"}
	for i, want := range wantIDs {
		switch b := first.Content[i].(type) {
		case llm.ReasoningItemBlock:
			if b.ID != want {
				t.Errorf("block[%d] id = %q, want %q", i, b.ID, want)
			}
		case llm.ToolUseBlock:
			if b.ID != want {
				t.Errorf("block[%d] id = %q, want %q", i, b.ID, want)
			}
		default:
			t.Errorf("block[%d] = %T, want reasoning_item or tool_use", i, b)
		}
	}
	if ri, ok := first.Content[0].(llm.ReasoningItemBlock); !ok || ri.EncryptedContent != "enc-0" {
		t.Errorf("block[0] encrypted content = %+v, want enc-0", first.Content[0])
	}
}

// newEchoRegistry is the shared echo-tool registry for persistence tests.
func newEchoRegistry() *tool.Registry {
	reg := tool.NewRegistry()
	reg.MustRegister(&mockTool{
		info:   tool.ToolInfo{Name: "echo", Description: "echoes"},
		result: tool.NewTextResult("echoed"),
	})
	return reg
}

// TestDeepCopyClonesReasoningItemBlocks pins that deepCopyMessages copies
// ReasoningItemBlock in both value and pointer forms, cloning the Summary
// slice so caller mutations cannot leak into the copied history.
func TestDeepCopyClonesReasoningItemBlocks(t *testing.T) {
	src := []llm.Message{{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		llm.ReasoningItemBlock{Type: "reasoning_item", ID: "rs_1", EncryptedContent: "enc", Summary: []string{"s"}},
		&llm.ReasoningItemBlock{Type: "reasoning_item", ID: "rs_2", EncryptedContent: "enc2"},
	}}}
	dst := deepCopyMessages(src)
	// Mutate the originals after the copy.
	src[0].Content[0].(llm.ReasoningItemBlock).Summary[0] = "mutated"
	*src[0].Content[1].(*llm.ReasoningItemBlock) = llm.ReasoningItemBlock{Type: "reasoning_item", ID: "clobbered"}
	if len(dst[0].Content) != 2 {
		t.Fatalf("copy has %d blocks, want 2", len(dst[0].Content))
	}
	vb := dst[0].Content[0].(llm.ReasoningItemBlock)
	if vb.Summary[0] != "s" {
		t.Errorf("value-form Summary leaked: %q", vb.Summary[0])
	}
	pb := dst[0].Content[1].(*llm.ReasoningItemBlock)
	if pb.ID != "rs_2" {
		t.Errorf("pointer-form copy clobbered: %+v", pb)
	}
}
