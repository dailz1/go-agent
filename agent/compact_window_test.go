package agent

// The built-in summarization request is bounded by the round budget
// (issue #17): switching to a smaller model must not send the old,
// oversized span to the summarizer itself.

import (
	"context"
	"errors"
	"iter"
	"reflect"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

type summarizerCapture struct {
	requests [][]llm.Message
}

func (p *summarizerCapture) Name() string { return "capture" }

func (p *summarizerCapture) Chat(_ context.Context, messages []llm.Message, _ []tool.ToolInfo, _ ...llm.Option) (*llm.Message, *llm.Usage, error) {
	p.requests = append(p.requests, append([]llm.Message(nil), messages...))
	m := llm.AssistantMessage("summary")
	return &m, nil, nil
}

func (p *summarizerCapture) ChatStream(context.Context, []llm.Message, []tool.ToolInfo, ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	return nil, llm.ErrStreamingNotSupported
}

func summarizationFixture() []llm.Message {
	return []llm.Message{
		llm.SystemMessage("sys"),
		llm.UserMessage("first"),
		llm.AssistantMessage(strings.Repeat("a", 200)),
		llm.UserMessage(strings.Repeat("u", 3000)),
		llm.AssistantMessage(strings.Repeat("b", 200)),
		llm.UserMessage("latest"),
	}
}

func TestSummarizationRequestBoundedByBudget(t *testing.T) {
	p := &summarizerCapture{}
	c := NewSummarizationCompactor()
	result, err := c.Compact(context.Background(), summarizationFixture(), CompactionBudget{MaxRunes: 1500}, p, nil)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if !result.Changed {
		t.Fatal("want adopted summary")
	}
	if len(p.requests) != 1 {
		t.Fatalf("summarizer calls = %d, want 1", len(p.requests))
	}
	if got := requestRunes(p.requests[0]); got > 1500 {
		t.Fatalf("summarization request = %d runes, want bounded to budget 1500", got)
	}
}

func TestSummarizationTinyBudgetCollapsesSpan(t *testing.T) {
	p := &summarizerCapture{}
	c := NewSummarizationCompactor()
	result, err := c.Compact(context.Background(), summarizationFixture(), CompactionBudget{MaxRunes: 100}, p, nil)
	if !errors.Is(err, ErrCompactionBudgetExceeded) {
		t.Fatalf("Compact error = %v, want tolerated budget sentinel", err)
	}
	if result.Changed {
		t.Fatal("degenerate budget must not adopt a summary of nothing")
	}
	if len(p.requests) != 0 {
		t.Fatalf("summarizer calls = %d, want 0 (base alone exceeds the budget)", len(p.requests))
	}
	if !reflect.DeepEqual(result.History, summarizationFixture()) {
		t.Fatal("degenerate budget must leave history untouched")
	}
}
