package agent

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

// Pins the issue #9 compactor isolation contract: the in-loop compactor
// receives a deep copy of live history, validation compares against the
// untouched original, and adoption deep-copies the returned history.

const isolationSystemPrompt = "keep me"

func isolationSeed() []llm.Message {
	return []llm.Message{
		llm.SystemMessage(isolationSystemPrompt),
		llm.UserMessage("hello"),
	}
}

func mutateSystemInPlace(history []llm.Message) bool {
	block, ok := history[0].Content[0].(llm.TextBlock)
	if !ok {
		return false
	}
	block.Text = "MUTATED"
	history[0].Content[0] = block
	return true
}

func firstMessageText(history []llm.Message) string {
	if len(history) == 0 {
		return ""
	}
	for _, block := range history[0].Content {
		switch tb := block.(type) {
		case llm.TextBlock:
			return tb.Text
		case *llm.TextBlock:
			if tb != nil {
				return tb.Text
			}
		}
	}
	return ""
}

func runWithIsolationCompactor(t *testing.T, compactor Compactor) (*RunResult, [][]llm.Message) {
	t.Helper()
	var requests [][]llm.Message
	provider := &messageCapturingMock{
		inner: NewMockProvider(MsgResponse(llm.AssistantMessage("done"))),
		onChat: func(messages []llm.Message) {
			requests = append(requests, copyMessages(messages))
		},
	}
	seed := isolationSeed()
	ag := New(
		provider,
		tool.NewRegistry(),
		WithSystemPrompt(isolationSystemPrompt),
		WithCompactor(compactor),
		WithCompactionThresholdPercent(100),
		WithContextWindowTokens(estimateRunes(seed)-1),
		WithLogger(discardLogger()),
	)
	result, err := ag.Run(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	return result, requests
}

func assertUntouchedOriginal(t *testing.T, result *RunResult, requests [][]llm.Message) {
	t.Helper()
	if len(requests) != 1 {
		t.Fatalf("provider requests = %d, want 1", len(requests))
	}
	if !reflect.DeepEqual(requests[0], isolationSeed()) {
		t.Errorf("provider request = %#v, want untouched original history", requests[0])
	}
	if got := firstMessageText(result.History); got != isolationSystemPrompt {
		t.Errorf("final history system %q, want %q", got, isolationSystemPrompt)
	}
}

func TestCompactorMutationCannotLeakOnError(t *testing.T) {
	invoked := false
	compactor := CompactFunc(func(_ context.Context, history []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
		invoked = true
		if !mutateSystemInPlace(history) {
			t.Errorf("compactor input system block not in expected value form")
		}
		return CompactionResult{}, errors.New("summarizer down")
	})
	result, requests := runWithIsolationCompactor(t, compactor)
	if !invoked {
		t.Fatal("compactor was not invoked")
	}
	assertUntouchedOriginal(t, result, requests)
}

func TestCompactorMutationCannotLeakOnUnchanged(t *testing.T) {
	compactor := CompactFunc(func(_ context.Context, history []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
		if !mutateSystemInPlace(history) {
			t.Errorf("compactor input system block not in expected value form")
		}
		return CompactionResult{History: history}, nil
	})
	result, requests := runWithIsolationCompactor(t, compactor)
	assertUntouchedOriginal(t, result, requests)
}

func TestCompactorMutationCannotLeakOnChanged(t *testing.T) {
	compactor := CompactFunc(func(_ context.Context, history []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
		if !mutateSystemInPlace(history) {
			t.Errorf("compactor input system block not in expected value form")
		}
		return CompactionResult{History: history, Changed: true, Strategies: []string{"mutator"}}, nil
	})
	result, requests := runWithIsolationCompactor(t, compactor)
	assertUntouchedOriginal(t, result, requests)
	if len(result.History) != 3 {
		t.Errorf("final history length = %d, want 3 (seed + assistant; mutated result must not be adopted)", len(result.History))
	}
}

func TestWellBehavedCompactorStillAdopts(t *testing.T) {
	compacted := []llm.Message{
		llm.SystemMessage(isolationSystemPrompt),
		llm.UserMessage("hi"),
	}
	compactor := CompactFunc(func(_ context.Context, history []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
		return CompactionResult{
			History:    deepCopyMessages(compacted),
			Changed:    true,
			Strategies: []string{"fresh"},
		}, nil
	})
	result, requests := runWithIsolationCompactor(t, compactor)
	if len(requests) != 1 {
		t.Fatalf("provider requests = %d, want 1", len(requests))
	}
	if !reflect.DeepEqual(requests[0], compacted) {
		t.Errorf("provider request = %#v, want adopted compacted history", requests[0])
	}
	if got := firstMessageText(result.History); got != isolationSystemPrompt {
		t.Errorf("final history system %q, want %q", got, isolationSystemPrompt)
	}
}

func TestCopyMessagesIsolatesEmptyContentBacking(t *testing.T) {
	src := []llm.Message{{Role: llm.RoleUser, Content: make([]llm.ContentBlock, 0, 4)}}
	dst := deepCopyMessages(src)
	dst[0].Content = append(dst[0].Content, llm.TextBlock{Type: "text", Text: "appended"})
	if len(src[0].Content) != 0 {
		t.Fatalf("source Content length = %d, want 0", len(src[0].Content))
	}
	if cap(src[0].Content) > 0 {
		if leaked, ok := src[0].Content[:1][0].(llm.TextBlock); ok && leaked.Text == "appended" {
			t.Errorf("destination append leaked into source backing array")
		}
	}
}

type wrappedTextBlock struct{ *llm.TextBlock }

func TestCompactionSkippedForUnsupportedInputBlocks(t *testing.T) {
	invoked := false
	compactor := CompactFunc(func(_ context.Context, _ []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
		invoked = true
		return CompactionResult{}, nil
	})
	var requests [][]llm.Message
	provider := &messageCapturingMock{
		inner: NewMockProvider(MsgResponse(llm.AssistantMessage("done"))),
		onChat: func(messages []llm.Message) {
			requests = append(requests, copyMessages(messages))
		},
	}
	history := []llm.Message{
		llm.SystemMessage(isolationSystemPrompt),
		{Role: llm.RoleUser, Content: []llm.ContentBlock{wrappedTextBlock{&llm.TextBlock{Type: "text", Text: "wrapped"}}}},
	}
	window := estimateRunes(history) + estimateRunes([]llm.Message{llm.UserMessage("hello")}) - 1
	ag := New(
		provider,
		tool.NewRegistry(),
		WithCompactor(compactor),
		WithCompactionThresholdPercent(100),
		WithContextWindowTokens(window),
		WithLogger(discardLogger()),
	)
	if _, err := ag.RunWithHistory(context.Background(), history, "hello"); err != nil {
		t.Fatalf("RunWithHistory() error = %v", err)
	}
	if invoked {
		t.Errorf("compactor invoked for history with unsupported block types, want skipped")
	}
	if len(requests) != 1 {
		t.Fatalf("provider requests = %d, want 1", len(requests))
	}
	if len(requests[0]) != 3 {
		t.Errorf("provider request length = %d, want 3 (history passed through un-compacted)", len(requests[0]))
	}
}

func TestCompactionRejectsUnsupportedResultBlocks(t *testing.T) {
	compactor := CompactFunc(func(_ context.Context, history []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
		poisoned := deepCopyMessages(history)
		poisoned[0].Content = []llm.ContentBlock{wrappedTextBlock{&llm.TextBlock{Type: "text", Text: isolationSystemPrompt}}}
		return CompactionResult{History: poisoned, Changed: true}, nil
	})
	var requests [][]llm.Message
	provider := &messageCapturingMock{
		inner: NewMockProvider(MsgResponse(llm.AssistantMessage("done"))),
		onChat: func(messages []llm.Message) {
			requests = append(requests, copyMessages(messages))
		},
	}
	seed := isolationSeed()
	ag := New(
		provider,
		tool.NewRegistry(),
		WithSystemPrompt(isolationSystemPrompt),
		WithCompactor(compactor),
		WithCompactionThresholdPercent(100),
		WithContextWindowTokens(estimateRunes(seed)-1),
		WithLogger(discardLogger()),
	)
	if _, err := ag.Run(context.Background(), "hello"); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(requests) != 1 {
		t.Fatalf("provider requests = %d, want 1", len(requests))
	}
	if !reflect.DeepEqual(requests[0], isolationSeed()) {
		t.Errorf("provider request = %#v, want untouched original history (result with unsupported blocks must be rejected)", requests[0])
	}
}

func TestCompactionFallbackPathHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	compactor := CompactFunc(func(_ context.Context, history []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
		poisoned := deepCopyMessages(history)
		poisoned[0].Content = []llm.ContentBlock{wrappedTextBlock{&llm.TextBlock{Type: "text", Text: isolationSystemPrompt}}}
		cancel()
		return CompactionResult{History: poisoned, Changed: true}, nil
	})
	var requests int
	provider := &messageCapturingMock{
		inner: NewMockProvider(MsgResponse(llm.AssistantMessage("done"))),
		onChat: func(messages []llm.Message) {
			requests++
		},
	}
	seed := isolationSeed()
	ag := New(
		provider,
		tool.NewRegistry(),
		WithSystemPrompt(isolationSystemPrompt),
		WithCompactor(compactor),
		WithCompactionThresholdPercent(100),
		WithContextWindowTokens(estimateRunes(seed)-1),
		WithLogger(discardLogger()),
	)
	if _, err := ag.Run(ctx, "hello"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if requests != 0 {
		t.Errorf("provider calls = %d, want 0 (canceled context must not reach the provider)", requests)
	}
}

func TestValidateCompactedRejectsMutatedCompaction(t *testing.T) {
	original := isolationSeed()
	compacted := deepCopyMessages(original)
	if !mutateSystemInPlace(compacted) {
		t.Fatal("mutation helper failed")
	}
	if err := validateCompacted(original, compacted); !errors.Is(err, ErrInvalidHistory) {
		t.Fatalf("validateCompacted() error = %v, want ErrInvalidHistory", err)
	}
}

func TestCompactorChainIsolatesFailedStageMutations(t *testing.T) {
	var stageBInput []llm.Message
	stageA := CompactFunc(func(_ context.Context, history []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
		if !mutateSystemInPlace(history) {
			t.Errorf("stage A input system block not in expected value form")
		}
		return CompactionResult{}, errors.New("stage A failed")
	})
	stageB := CompactFunc(func(_ context.Context, history []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
		stageBInput = deepCopyMessages(history)
		return CompactionResult{History: deepCopyMessages(isolationSeed()), Changed: true}, nil
	})
	chain := NewCompactorChain(stageA, stageB)
	big := append(isolationSeed(), llm.AssistantMessage(strings.Repeat("x", 4096)))
	result, err := chain.Compact(context.Background(), big, CompactionBudget{MaxRunes: estimateRunes(isolationSeed())}, nil, nil)
	if err != nil {
		t.Fatalf("chain Compact() error = %v", err)
	}
	if len(result.Errors) != 1 {
		t.Errorf("chain errors = %v, want 1 (stage A)", result.Errors)
	}
	if got := firstMessageText(stageBInput); got != isolationSystemPrompt {
		t.Errorf("stage B received system %q, want %q (stage A mutation must not propagate)", got, isolationSystemPrompt)
	}
	if got := firstMessageText(result.History); got != isolationSystemPrompt {
		t.Errorf("adopted result system %q, want %q", got, isolationSystemPrompt)
	}
}

func TestAdoptedHistoryIsolatedFromCompactorRetention(t *testing.T) {
	retained := []llm.Message{
		llm.SystemMessage(isolationSystemPrompt),
		llm.UserMessage("hi"),
	}
	compactor := CompactFunc(func(_ context.Context, _ []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
		return CompactionResult{History: retained, Changed: true}, nil
	})
	var requests [][]llm.Message
	provider := &messageCapturingMock{
		inner: NewMockProvider(MsgResponse(llm.AssistantMessage("done"))),
		onChat: func(messages []llm.Message) {
			requests = append(requests, copyMessages(messages))
		},
	}
	seed := isolationSeed()
	ag := New(
		provider,
		tool.NewRegistry(),
		WithSystemPrompt(isolationSystemPrompt),
		WithCompactor(compactor),
		WithCompactionThresholdPercent(100),
		WithContextWindowTokens(estimateRunes(seed)-1),
		WithLogger(discardLogger()),
	)
	stream, err := ag.RunStream(context.Background(), "hello")
	if err != nil {
		t.Fatalf("RunStream() error = %v", err)
	}
	for event, evErr := range stream {
		if evErr != nil {
			t.Fatalf("RunStream() error = %v", evErr)
		}
		if _, ok := event.(CompactionEvent); ok {
			block, ok := retained[1].Content[0].(llm.TextBlock)
			if !ok {
				t.Fatalf("retained user block not in expected value form")
			}
			block.Text = "POISONED"
			retained[1].Content[0] = block
		}
	}
	if len(requests) == 0 {
		t.Fatal("provider was not called")
	}
	last := requests[len(requests)-1]
	got, ok := last[1].Content[0].(llm.TextBlock)
	if !ok {
		t.Fatalf("provider user block not in expected value form")
	}
	if got.Text != "hi" {
		t.Errorf("provider saw user text %q, want %q (retained mutation must not reach adopted history)", got.Text, "hi")
	}
}

func TestUnchangedMutationCannotShrinkLiveHistory(t *testing.T) {
	compactor := CompactFunc(func(_ context.Context, history []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
		block, ok := history[1].Content[0].(llm.TextBlock)
		if !ok {
			t.Errorf("compactor input user block not in expected value form")
			return CompactionResult{History: history}, nil
		}
		block.Text = "x"
		history[1].Content[0] = block
		return CompactionResult{History: history}, nil
	})
	var requests [][]llm.Message
	provider := &messageCapturingMock{
		inner: NewMockProvider(MsgResponse(llm.AssistantMessage("done"))),
		onChat: func(messages []llm.Message) {
			requests = append(requests, copyMessages(messages))
		},
	}
	seed := isolationSeed()
	ag := New(
		provider,
		tool.NewRegistry(),
		WithSystemPrompt(isolationSystemPrompt),
		WithCompactor(compactor),
		WithCompactionThresholdPercent(100),
		WithContextWindowTokens(estimateRunes(seed)-1),
		WithLogger(discardLogger()),
	)
	_, err := ag.Run(context.Background(), "hello")
	if !errors.Is(err, ErrCompactionBudgetExceeded) {
		t.Fatalf("Run() error = %v, want ErrCompactionBudgetExceeded (live history unchanged, budget still exceeded)", err)
	}
	if len(requests) != 0 {
		t.Errorf("provider requests = %d, want 0 (run failed before any request)", len(requests))
	}
}
