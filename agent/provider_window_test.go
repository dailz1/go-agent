package agent

// Kernel behavior tests for the effective context window (issue #17,
// docs/DESIGN.md P1「模型上下文窗口声明与有效窗口」): a thread that
// hot-switches to a provider-declared smaller model must retarget
// compaction before the request goes out, and every budget derived from
// the window must follow the round's effective W.

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

type windowedProvider struct {
	name         string
	windows      map[string]int
	noCapability bool
	steps        [][]llm.Chunk
	received     [][]llm.Message
}

func (p *windowedProvider) Name() string { return p.name }

func (p *windowedProvider) ContextWindow(model string) (int, bool) {
	if p.noCapability || model == "" {
		return 0, false
	}
	tokens, ok := p.windows[model]
	return tokens, ok
}

func (p *windowedProvider) Chat(context.Context, []llm.Message, []tool.ToolInfo, ...llm.Option) (*llm.Message, *llm.Usage, error) {
	return nil, nil, llm.ErrStreamingNotSupported
}

func (p *windowedProvider) ChatStream(_ context.Context, messages []llm.Message, _ []tool.ToolInfo, _ ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	p.received = append(p.received, append([]llm.Message(nil), messages...))
	if len(p.steps) == 0 {
		return nil, fmt.Errorf("unexpected ChatStream call on %s", p.name)
	}
	chunks := p.steps[0]
	p.steps = p.steps[1:]
	return func(yield func(llm.Chunk, error) bool) {
		for _, chunk := range chunks {
			if !yield(chunk, nil) {
				return
			}
		}
	}, nil
}

func windowedText(text string) []llm.Chunk {
	return []llm.Chunk{llm.TextDeltaChunk{Text: text}, llm.DoneChunk{FinishReason: "stop"}}
}

func requestRunes(messages []llm.Message) int {
	total := 0
	for _, m := range messages {
		for _, b := range m.Content {
			if tb, ok := b.(llm.TextBlock); ok {
				total += utf8.RuneCountInString(tb.Text)
			}
		}
	}
	return total
}

func windowPtr(s string) *string { return &s }

func TestContextWindowShrinkCompactsBeforeRequest(t *testing.T) {
	ctx := context.Background()
	pa := &windowedProvider{name: "pa", windows: map[string]int{"big-model": 100000},
		steps: [][]llm.Chunk{windowedText(strings.Repeat("a", 3000))}}
	pb := &windowedProvider{name: "pb", windows: map[string]int{"small-model": 2000},
		steps: [][]llm.Chunk{windowedText("small ok")}}
	st := store.NewMemory()
	ag := New(pa, tool.NewRegistry(),
		WithStore(st),
		WithProviders(pb),
		WithContextWindowTokens(100000),
		WithLLMOptions(llm.WithModel("big-model")),
		WithMaxIter(3),
	)

	if _, err := ag.Run(ctx, strings.Repeat("u", 1200), WithThread("t-w")); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if err := SetThreadConfig(ctx, st, "t-w", ConfigChange{
		Provider: windowPtr("pb"),
		Model:    windowPtr("small-model"),
	}); err != nil {
		t.Fatalf("switch: %v", err)
	}
	if _, err := ag.Run(ctx, "go", WithThread("t-w")); err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if len(pb.received) != 1 {
		t.Fatalf("pb calls = %d, want 1", len(pb.received))
	}
	if got := requestRunes(pb.received[0]); got > 1600 {
		t.Fatalf("request runes = %d, want compacted to <= target 1600", got)
	}
	if len(pb.received[0]) != 2 {
		t.Fatalf("request messages = %d, want uncompacted 3 shrunk to 2", len(pb.received[0]))
	}
}

func TestContextWindowGrowCappedByConstructor(t *testing.T) {
	ctx := context.Background()
	pa := &windowedProvider{name: "pa", windows: map[string]int{"big-model": 100000},
		steps: [][]llm.Chunk{windowedText("a")}}
	pb := &windowedProvider{name: "pb", windows: map[string]int{"huge-model": 1000000},
		steps: [][]llm.Chunk{windowedText("b")}}
	st := store.NewMemory()
	ag := New(pa, tool.NewRegistry(),
		WithStore(st),
		WithProviders(pb),
		WithContextWindowTokens(8192),
		WithLLMOptions(llm.WithModel("big-model")),
		WithMaxIter(3),
	)

	if _, err := ag.Run(ctx, "u", WithThread("t-g")); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if err := SetThreadConfig(ctx, st, "t-g", ConfigChange{
		Provider: windowPtr("pb"),
		Model:    windowPtr("huge-model"),
	}); err != nil {
		t.Fatalf("switch: %v", err)
	}
	if _, err := ag.Run(ctx, "go", WithThread("t-g")); err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if len(pb.received[0]) != 3 {
		t.Fatalf("request messages = %d, want full uncompacted history 3", len(pb.received[0]))
	}
}

func TestSameProviderModelSwitchRetargets(t *testing.T) {
	ctx := context.Background()
	pm := &windowedProvider{name: "pm", windows: map[string]int{"big-model": 100000, "small-model": 2000},
		steps: [][]llm.Chunk{
			windowedText(strings.Repeat("a", 3000)),
			windowedText("small ok"),
		}}
	st := store.NewMemory()
	ag := New(pm, tool.NewRegistry(),
		WithStore(st),
		WithContextWindowTokens(100000),
		WithLLMOptions(llm.WithModel("big-model")),
		WithMaxIter(3),
	)

	if _, err := ag.Run(ctx, strings.Repeat("u", 1200), WithThread("t-m")); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if err := SetThreadConfig(ctx, st, "t-m", ConfigChange{Model: windowPtr("small-model")}); err != nil {
		t.Fatalf("switch: %v", err)
	}
	if _, err := ag.Run(ctx, "go", WithThread("t-m")); err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if len(pm.received) != 2 {
		t.Fatalf("pm calls = %d, want 2", len(pm.received))
	}
	if got := requestRunes(pm.received[1]); got > 1600 {
		t.Fatalf("request runes = %d, want compacted to <= target 1600", got)
	}
}

func TestUndeclaredWindowFallsBack(t *testing.T) {
	ctx := context.Background()
	pa := &windowedProvider{name: "pa", noCapability: true,
		steps: [][]llm.Chunk{windowedText("a")}}
	st := store.NewMemory()
	ag := New(pa, tool.NewRegistry(),
		WithStore(st),
		WithContextWindowTokens(8192),
		WithMaxIter(3),
	)
	res, err := ag.Run(ctx, strings.Repeat("u", 100), WithThread("t-u"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(pa.received) != 1 || len(pa.received[0]) != 1 {
		t.Fatalf("history must pass through unchanged, got %d messages", len(pa.received[0]))
	}
	if res.Message.Content == nil {
		t.Fatal("missing final message")
	}
}

func TestCarryCappedAtEffectiveWindow(t *testing.T) {
	ctx := context.Background()
	pa := &windowedProvider{name: "pa", windows: map[string]int{"big-model": 100000},
		steps: [][]llm.Chunk{windowedText("a")}}
	pb := &windowedProvider{name: "pb", windows: map[string]int{"small-model": 2000},
		steps: [][]llm.Chunk{windowedText("injected ok")}}
	st := store.NewMemory()
	ag := New(pa, tool.NewRegistry(),
		WithStore(st),
		WithProviders(pb),
		WithContextWindowTokens(100000),
		WithLLMOptions(llm.WithModel("big-model")),
		WithMaxIter(3),
	)

	if _, err := ag.EnqueueInputWithID(ctx, "t-c", "k1", strings.Repeat("s", 50000)); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := ag.Run(ctx, "start", WithThread("t-c")); err != nil {
		t.Fatalf("run 1 (big-window drain): %v", err)
	}
	if err := SetThreadConfig(ctx, st, "t-c", ConfigChange{
		Provider: windowPtr("pb"),
		Model:    windowPtr("small-model"),
	}); err != nil {
		t.Fatalf("switch: %v", err)
	}
	if _, err := ag.Run(ctx, "go", WithThread("t-c")); err != nil {
		t.Fatalf("run 2 (shrink round): %v", err)
	}
	if got := requestRunes(pb.received[0]); got > 2000 {
		t.Fatalf("shrink round request = %d runes, must stay within W 2000", got)
	}
	_, err := ag.Run(ctx, strings.Repeat("n", 2500), WithThread("t-c"))
	if !errors.Is(err, ErrCompactionBudgetExceeded) {
		t.Fatalf("run 3: want fail-fast %v on oversized protected history under stale carry, got %v", ErrCompactionBudgetExceeded, err)
	}
}
