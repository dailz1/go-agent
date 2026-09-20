package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

func TestAccum_SingleToolCall(t *testing.T) {
	var a toolCallAccum
	a.feed(llm.ToolCallStartChunk{Index: 0, ID: "c1", Name: "tool_a"})
	a.feed(llm.ToolCallArgsChunk{Index: 0, Delta: `{"key":"val"}`})

	blocks, err := a.assemble()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	b := blocks[0]
	if b.Type != "tool_use" {
		t.Errorf("Type = %q, want %q", b.Type, "tool_use")
	}
	if b.ID != "c1" {
		t.Errorf("ID = %q, want %q", b.ID, "c1")
	}
	if b.Name != "tool_a" {
		t.Errorf("Name = %q, want %q", b.Name, "tool_a")
	}
	if string(b.Input) != `{"key":"val"}` {
		t.Errorf("Input = %q, want %q", string(b.Input), `{"key":"val"}`)
	}
}

func TestAccum_InterleavedMultiTool(t *testing.T) {
	var a toolCallAccum
	a.feed(llm.ToolCallStartChunk{Index: 0, ID: "c1", Name: "tool_a"})
	a.feed(llm.ToolCallStartChunk{Index: 1, ID: "c2", Name: "tool_b"})
	a.feed(llm.ToolCallArgsChunk{Index: 0, Delta: `{"a`})
	a.feed(llm.ToolCallArgsChunk{Index: 1, Delta: `{"b`})
	a.feed(llm.ToolCallArgsChunk{Index: 0, Delta: `":1}`})
	a.feed(llm.ToolCallArgsChunk{Index: 1, Delta: `":2}`})

	blocks, err := a.assemble()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks))
	}
	if blocks[0].Name != "tool_a" {
		t.Errorf("blocks[0].Name = %q, want %q", blocks[0].Name, "tool_a")
	}
	if string(blocks[0].Input) != `{"a":1}` {
		t.Errorf("blocks[0].Input = %q, want %q", string(blocks[0].Input), `{"a":1}`)
	}
	if blocks[1].Name != "tool_b" {
		t.Errorf("blocks[1].Name = %q, want %q", blocks[1].Name, "tool_b")
	}
	if string(blocks[1].Input) != `{"b":2}` {
		t.Errorf("blocks[1].Input = %q, want %q", string(blocks[1].Input), `{"b":2}`)
	}
}

func TestAccum_DuplicateToolCallStart(t *testing.T) {
	tests := []struct {
		name   string
		chunks []llm.Chunk
	}{
		{
			name: "replayed before args",
			chunks: []llm.Chunk{
				llm.ToolCallStartChunk{Index: 0, ID: "c1", Name: "tool_a"},
				llm.ToolCallStartChunk{Index: 0, ID: "c1", Name: "tool_a"},
			},
		},
		{
			name: "replayed after args",
			chunks: []llm.Chunk{
				llm.ToolCallStartChunk{Index: 0, ID: "c1", Name: "tool_a"},
				llm.ToolCallArgsChunk{Index: 0, Delta: `{"key":"val"}`},
				llm.ToolCallStartChunk{Index: 0, ID: "c1", Name: "tool_a"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var a toolCallAccum
			for _, chunk := range tt.chunks {
				a.feed(chunk)
			}

			_, err := a.assemble()
			if err == nil {
				t.Fatal("assemble() error = nil, want duplicate index error")
			}
			if !strings.Contains(err.Error(), "duplicate tool call index 0") {
				t.Errorf("assemble() error = %q, want duplicate index 0", err)
			}
		})
	}
}

func TestAccum_EmptyFinish(t *testing.T) {
	var a toolCallAccum
	blocks, err := a.assemble()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if blocks != nil {
		t.Errorf("expected nil, got %v", blocks)
	}
}

func TestAccum_TextAccumulation(t *testing.T) {
	var a toolCallAccum
	a.feed(llm.TextDeltaChunk{Text: "Hello "})
	a.feed(llm.TextDeltaChunk{Text: "world"})

	blocks := a.textBlocks()
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	tb, ok := blocks[0].(llm.TextBlock)
	if !ok {
		t.Fatalf("expected TextBlock, got %T", blocks[0])
	}
	if tb.Type != "text" {
		t.Errorf("Type = %q, want %q", tb.Type, "text")
	}
	if tb.Text != "Hello world" {
		t.Errorf("Text = %q, want %q", tb.Text, "Hello world")
	}
}

func TestAccum_Reset(t *testing.T) {
	var a toolCallAccum
	a.feed(llm.ToolCallStartChunk{Index: 0, ID: "c1", Name: "tool_a"})
	a.feed(llm.TextDeltaChunk{Text: "hi"})

	a.reset()

	blocks, err := a.assemble()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if blocks != nil {
		t.Errorf("finish after reset: expected nil, got %v", blocks)
	}
	if tb := a.textBlocks(); tb != nil {
		t.Errorf("textBlocks after reset: expected nil, got %v", tb)
	}
}

func TestAccum_SortedFinish(t *testing.T) {
	var a toolCallAccum
	a.feed(llm.ToolCallStartChunk{Index: 5, ID: "c5", Name: "e"})
	a.feed(llm.ToolCallStartChunk{Index: 0, ID: "c0", Name: "a"})
	a.feed(llm.ToolCallArgsChunk{Index: 5, Delta: `5`})
	a.feed(llm.ToolCallArgsChunk{Index: 0, Delta: `0`})

	blocks, err := a.assemble()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks))
	}
	if blocks[0].Name != "a" {
		t.Errorf("blocks[0].Name = %q, want %q (Index 0)", blocks[0].Name, "a")
	}
	if blocks[0].ID != "c0" {
		t.Errorf("blocks[0].ID = %q, want %q", blocks[0].ID, "c0")
	}
	if blocks[1].Name != "e" {
		t.Errorf("blocks[1].Name = %q, want %q (Index 5)", blocks[1].Name, "e")
	}
	if blocks[1].ID != "c5" {
		t.Errorf("blocks[1].ID = %q, want %q", blocks[1].ID, "c5")
	}
}

func TestAccum_DoneChunkIgnored(t *testing.T) {
	var a toolCallAccum
	a.feed(llm.DoneChunk{FinishReason: "stop"})

	blocks, err := a.assemble()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if blocks != nil {
		t.Errorf("expected nil from finish, got %v", blocks)
	}
	if tb := a.textBlocks(); tb != nil {
		t.Errorf("expected nil from textBlocks, got %v", tb)
	}
}

func TestAccum_DoneChunkUsageNil(t *testing.T) {
	var a toolCallAccum
	a.feed(llm.DoneChunk{FinishReason: "stop"})

	if u := a.usage(); u != nil {
		t.Errorf("usage() = %v, want nil when DoneChunk has no Usage", u)
	}
}

func TestAccum_DoneChunkUsageCaptured(t *testing.T) {
	var a toolCallAccum
	a.feed(llm.DoneChunk{
		FinishReason: "stop",
		Usage:        &llm.Usage{InputTokens: 42, OutputTokens: 7},
	})

	u := a.usage()
	if u == nil {
		t.Fatal("usage() = nil, want non-nil")
	}
	if u.InputTokens != 42 {
		t.Errorf("InputTokens = %d, want 42", u.InputTokens)
	}
	if u.OutputTokens != 7 {
		t.Errorf("OutputTokens = %d, want 7", u.OutputTokens)
	}
}

func TestAccum_DualDoneChunk_SecondWins(t *testing.T) {
	var a toolCallAccum

	// First DoneChunk with nil Usage (OpenAI sometimes sends an early done)
	a.feed(llm.DoneChunk{FinishReason: "stop", Usage: nil})
	if u := a.usage(); u != nil {
		t.Errorf("after first DoneChunk: usage() = %v, want nil", u)
	}

	// Second DoneChunk with actual Usage
	a.feed(llm.DoneChunk{
		FinishReason: "stop",
		Usage:        &llm.Usage{InputTokens: 100, OutputTokens: 50, ReasoningTokens: 20},
	})

	u := a.usage()
	if u == nil {
		t.Fatal("after second DoneChunk: usage() = nil, want non-nil")
	}
	if u.InputTokens != 100 {
		t.Errorf("InputTokens = %d, want 100", u.InputTokens)
	}
	if u.OutputTokens != 50 {
		t.Errorf("OutputTokens = %d, want 50", u.OutputTokens)
	}
	if u.ReasoningTokens != 20 {
		t.Errorf("ReasoningTokens = %d, want 20", u.ReasoningTokens)
	}
}

func TestAccum_ResetClearsUsage(t *testing.T) {
	var a toolCallAccum
	a.feed(llm.DoneChunk{
		FinishReason: "stop",
		Usage:        &llm.Usage{InputTokens: 42},
	})

	if u := a.usage(); u == nil {
		t.Fatal("before reset: usage() = nil, want non-nil")
	}

	a.reset()

	if u := a.usage(); u != nil {
		t.Errorf("after reset: usage() = %v, want nil", u)
	}
}

func TestAccum_UsageAfterResetReaccumulates(t *testing.T) {
	var a toolCallAccum

	// First iteration: accumulate usage then reset
	a.feed(llm.DoneChunk{FinishReason: "stop", Usage: &llm.Usage{InputTokens: 10}})
	a.reset()

	// Second iteration: new usage
	a.feed(llm.DoneChunk{FinishReason: "stop", Usage: &llm.Usage{InputTokens: 20}})

	u := a.usage()
	if u == nil {
		t.Fatal("usage() = nil after second iteration")
	}
	if u.InputTokens != 20 {
		t.Errorf("InputTokens = %d, want 20 (not leaked from first iteration)", u.InputTokens)
	}
}

func TestAccum_OrphanArgsDetected(t *testing.T) {
	var a toolCallAccum
	a.feed(llm.ToolCallArgsChunk{Index: 5, Delta: `{"key":"val"}`})
	a.feed(llm.ToolCallArgsChunk{Index: 2, Delta: `{"a":"b"}`})

	_, err := a.assemble()
	if err == nil {
		t.Fatal("expected error for orphan args, got nil")
	}
	if !strings.Contains(err.Error(), "orphan") {
		t.Errorf("error = %q, want mention of orphan", err.Error())
	}
	if !strings.Contains(err.Error(), "2") || !strings.Contains(err.Error(), "5") {
		t.Errorf("error = %q, want mention of indices 2 and 5", err.Error())
	}
}

func TestAccum_MissingID(t *testing.T) {
	var a toolCallAccum
	a.calls = map[int]*callAccum{
		0: {id: "", name: "search"},
	}

	_, err := a.assemble()
	if err == nil {
		t.Fatal("expected error for missing id, got nil")
	}
	if !strings.Contains(err.Error(), "missing id") {
		t.Errorf("error = %q, want mention of missing id", err.Error())
	}
}

func TestAccum_MissingName(t *testing.T) {
	var a toolCallAccum
	a.calls = map[int]*callAccum{
		0: {id: "c1", name: ""},
	}

	_, err := a.assemble()
	if err == nil {
		t.Fatal("expected error for missing name, got nil")
	}
	if !strings.Contains(err.Error(), "missing name") {
		t.Errorf("error = %q, want mention of missing name", err.Error())
	}
}

func TestAccum_OrphanArgsWithValidToolCalls(t *testing.T) {
	var a toolCallAccum
	a.feed(llm.ToolCallStartChunk{Index: 0, ID: "c1", Name: "search"})
	a.feed(llm.ToolCallArgsChunk{Index: 0, Delta: `{"q":"go"}`})
	a.feed(llm.ToolCallArgsChunk{Index: 3, Delta: `{"orphan":true}`})

	_, err := a.assemble()
	if err == nil {
		t.Fatal("expected error for orphan args, got nil")
	}
	if !strings.Contains(err.Error(), "orphan") {
		t.Errorf("error = %q, want mention of orphan", err.Error())
	}
}

func TestAccumulator_ReasoningDelta(t *testing.T) {
	var a toolCallAccum
	a.feed(llm.ReasoningDeltaChunk{Text: "step 1: "})
	a.feed(llm.ReasoningDeltaChunk{Text: "analyze"})

	blocks := a.reasoningBlocks()
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	rb, ok := blocks[0].(llm.ReasoningBlock)
	if !ok {
		t.Fatalf("expected ReasoningBlock, got %T", blocks[0])
	}
	if rb.Type != "reasoning" {
		t.Errorf("Type = %q, want %q", rb.Type, "reasoning")
	}
	if rb.Content != "step 1: analyze" {
		t.Errorf("Content = %q, want %q", rb.Content, "step 1: analyze")
	}
}

func TestAccumulator_ReasoningEmpty(t *testing.T) {
	var a toolCallAccum
	if blocks := a.reasoningBlocks(); blocks != nil {
		t.Errorf("expected nil, got %v", blocks)
	}
}

func TestAccumulator_ReasoningPrepended(t *testing.T) {
	var a toolCallAccum
	a.feed(llm.ReasoningDeltaChunk{Text: "thinking"})
	a.feed(llm.TextDeltaChunk{Text: "answer"})

	var contentBlocks []llm.ContentBlock
	contentBlocks = append(contentBlocks, a.reasoningBlocks()...)
	contentBlocks = append(contentBlocks, a.textBlocks()...)

	if len(contentBlocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(contentBlocks))
	}
	if _, ok := contentBlocks[0].(llm.ReasoningBlock); !ok {
		t.Errorf("block[0] = %T, want ReasoningBlock", contentBlocks[0])
	}
	if _, ok := contentBlocks[1].(llm.TextBlock); !ok {
		t.Errorf("block[1] = %T, want TextBlock", contentBlocks[1])
	}
}

func TestAccumulator_ReasoningReset(t *testing.T) {
	var a toolCallAccum
	a.feed(llm.ReasoningDeltaChunk{Text: "some reasoning"})

	a.reset()

	if blocks := a.reasoningBlocks(); blocks != nil {
		t.Errorf("expected nil after reset, got %v", blocks)
	}
}

func TestAccumulator_ReasoningWithToolCalls(t *testing.T) {
	var a toolCallAccum
	a.feed(llm.ReasoningDeltaChunk{Text: "need to search"})
	a.feed(llm.ToolCallStartChunk{Index: 0, ID: "c1", Name: "search"})
	a.feed(llm.ToolCallArgsChunk{Index: 0, Delta: `{"q":"test"}`})

	reasoning := a.reasoningBlocks()
	if len(reasoning) != 1 {
		t.Fatalf("expected 1 reasoning block, got %d", len(reasoning))
	}
	rb, ok := reasoning[0].(llm.ReasoningBlock)
	if !ok {
		t.Fatalf("expected ReasoningBlock, got %T", reasoning[0])
	}
	if rb.Content != "need to search" {
		t.Errorf("Content = %q, want %q", rb.Content, "need to search")
	}

	toolBlocks, err := a.assemble()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(toolBlocks) != 1 {
		t.Fatalf("expected 1 tool block, got %d", len(toolBlocks))
	}

	text := a.textBlocks()
	if text != nil {
		t.Errorf("expected nil text blocks, got %v", text)
	}
}

func TestAccumulator_TextPlusReasoningAccumulation(t *testing.T) {
	var a toolCallAccum
	a.feed(llm.ReasoningDeltaChunk{Text: "reasoning "})
	a.feed(llm.ReasoningDeltaChunk{Text: "content"})
	a.feed(llm.TextDeltaChunk{Text: "text "})
	a.feed(llm.TextDeltaChunk{Text: "output"})

	reasoning := a.reasoningBlocks()
	if len(reasoning) != 1 {
		t.Fatalf("expected 1 reasoning block, got %d", len(reasoning))
	}
	rb, _ := reasoning[0].(llm.ReasoningBlock)
	if rb.Content != "reasoning content" {
		t.Errorf("reasoning Content = %q, want %q", rb.Content, "reasoning content")
	}

	text := a.textBlocks()
	if len(text) != 1 {
		t.Fatalf("expected 1 text block, got %d", len(text))
	}
	tb, _ := text[0].(llm.TextBlock)
	if tb.Text != "text output" {
		t.Errorf("text Text = %q, want %q", tb.Text, "text output")
	}
}

func TestAccumulator_MultiIterationReset(t *testing.T) {
	var a toolCallAccum

	// Iteration N: reasoning + text
	a.feed(llm.ReasoningDeltaChunk{Text: "thinking"})
	a.feed(llm.TextDeltaChunk{Text: "response"})

	// Simulate end of iteration: reset
	a.reset()

	// Verify reasoning cleared
	if blocks := a.reasoningBlocks(); blocks != nil {
		t.Fatalf("reasoningBlocks after reset = %v, want nil", blocks)
	}
	// Verify text cleared
	if blocks := a.textBlocks(); blocks != nil {
		t.Fatalf("textBlocks after reset = %v, want nil", blocks)
	}

	// Iteration N+1: only tool calls (no reasoning leak)
	a.feed(llm.ToolCallStartChunk{Index: 0, ID: "c1", Name: "search"})
	a.feed(llm.ToolCallArgsChunk{Index: 0, Delta: `{"q":"go"}`})

	// reasoning should still be nil
	if blocks := a.reasoningBlocks(); blocks != nil {
		t.Errorf("reasoningBlocks leaked from previous iteration: %v", blocks)
	}
	// text should still be nil
	if blocks := a.textBlocks(); blocks != nil {
		t.Errorf("textBlocks leaked from previous iteration: %v", blocks)
	}
	// tool calls should work
	toolBlocks, err := a.assemble()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(toolBlocks) != 1 {
		t.Fatalf("expected 1 tool block, got %d", len(toolBlocks))
	}
	if toolBlocks[0].Name != "search" {
		t.Errorf("tool block Name = %q, want %q", toolBlocks[0].Name, "search")
	}
}

func TestRunStream_FeedsAllChunkTypes(t *testing.T) {
	provider := NewMockStreamingProvider([][]llm.Chunk{
		{
			llm.ReasoningDeltaChunk{Text: "let me think"},
			llm.TextDeltaChunk{Text: "hello"},
			llm.DoneChunk{FinishReason: "stop"},
		},
	})
	agent := New(provider, tool.NewRegistry(), WithLogger(discardLogger()))

	seq, err := agent.RunStream(context.Background(), "test")
	events, errs := collectEvents(t, seq, err)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	// Expect: ThinkingDeltaEvent, TextDeltaEvent, DoneEvent
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}

	td, ok := events[0].(ThinkingDeltaEvent)
	if !ok {
		t.Fatalf("events[0] = %T, want ThinkingDeltaEvent", events[0])
	}
	if td.Text != "let me think" {
		t.Errorf("ThinkingDeltaEvent.Text = %q, want %q", td.Text, "let me think")
	}

	txt, ok := events[1].(TextDeltaEvent)
	if !ok {
		t.Fatalf("events[1] = %T, want TextDeltaEvent", events[1])
	}
	if txt.Text != "hello" {
		t.Errorf("TextDeltaEvent.Text = %q, want %q", txt.Text, "hello")
	}

	done, ok := events[2].(DoneEvent)
	if !ok {
		t.Fatalf("events[2] = %T, want DoneEvent", events[2])
	}

	// Verify the reconstructed message has reasoning before text
	if len(done.Message.Content) != 2 {
		t.Fatalf("Message.Content length = %d, want 2", len(done.Message.Content))
	}
	if _, ok := done.Message.Content[0].(llm.ReasoningBlock); !ok {
		t.Errorf("Message.Content[0] = %T, want ReasoningBlock", done.Message.Content[0])
	}
	if _, ok := done.Message.Content[1].(llm.TextBlock); !ok {
		t.Errorf("Message.Content[1] = %T, want TextBlock", done.Message.Content[1])
	}
}
