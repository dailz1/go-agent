package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/pkg/llm"
	"github.com/dailz1/go-agent/pkg/tool"
)

func TestCompactorStrategiesObserveCancellation(t *testing.T) {
	huge := strings.Repeat("x", 1<<20)
	history := []llm.Message{
		llm.UserMessage("first"),
		llm.AssistantToolCallMessage(llm.ToolUseBlock{
			Type: "tool_use", ID: "old", Name: "lookup", Input: json.RawMessage(`{"query":"old"}`),
		}),
		llm.ToolResultMessage("old", tool.NewTextResult(huge)),
		llm.AssistantMessage(huge),
		llm.UserMessage("latest"),
		llm.AssistantMessage("final"),
	}

	strategies := []struct {
		name      string
		compactor Compactor
	}{
		{name: "drop oldest tool groups", compactor: NewDropOldestToolGroupsCompactor()},
		{name: "sliding window", compactor: NewSlidingWindowCompactor()},
	}

	for _, tt := range strategies {
		t.Run(tt.name, func(t *testing.T) {
			t.Run("canceled", func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()

				result, err := tt.compactor.Compact(ctx, history, CompactionBudget{MaxRunes: 0})
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("Compact() error = %v, want context.Canceled", err)
				}
				foundCancellation := false
				for _, resultErr := range result.Errors {
					if errors.Is(resultErr, context.Canceled) {
						foundCancellation = true
						break
					}
				}
				if !foundCancellation {
					t.Errorf("Compact() Errors = %v, want context.Canceled", result.Errors)
				}
				if len(result.History) == 0 || len(result.History) > len(history) {
					t.Fatalf("Compact() returned invalid partial history length %d", len(result.History))
				}
				if _, partitionErr := partitionHistory(result.History); partitionErr != nil {
					t.Errorf("Compact() returned invalid partial history: %v", partitionErr)
				}
			})

			t.Run("uncanceled", func(t *testing.T) {
				small := []llm.Message{llm.UserMessage("question"), llm.AssistantMessage("answer")}
				result, err := tt.compactor.Compact(
					context.Background(),
					small,
					CompactionBudget{MaxRunes: estimateRunes(small)},
				)
				if err != nil {
					t.Fatalf("Compact() unexpected error: %v", err)
				}
				if !reflect.DeepEqual(result.History, small) || result.Changed || len(result.Errors) != 0 {
					t.Errorf("Compact() result = %#v, want unchanged history", result)
				}
			})
		})
	}
}

func TestAgentCompaction(t *testing.T) {
	t.Run("below and equal threshold do not compact", func(t *testing.T) {
		base := []llm.Message{llm.UserMessage("kept history")}
		current := append(copyMessages(base), llm.UserMessage("next"))
		for _, tt := range []struct {
			name   string
			window int
		}{
			{name: "equal threshold", window: estimateRunes(current)},
			{name: "below threshold", window: estimateRunes(current) + 1},
		} {
			t.Run(tt.name, func(t *testing.T) {
				calls := 0
				compactor := CompactFunc(func(_ context.Context, history []llm.Message, _ CompactionBudget) (CompactionResult, error) {
					calls++
					return CompactionResult{History: history}, nil
				})
				requests := [][]llm.Message{}
				provider := &messageCapturingMock{
					inner: NewMockProvider(MsgResponse(llm.AssistantMessage("done"))),
					onChat: func(messages []llm.Message) {
						requests = append(requests, copyMessages(messages))
					},
				}
				agent := New(
					provider,
					tool.NewRegistry(),
					WithCompactor(compactor),
					WithCompactionThresholdPercent(100),
					WithContextWindowTokens(tt.window),
					WithLogger(discardLogger()),
				)

				seq, err := agent.RunStreamWithHistory(context.Background(), base, "next")
				events, errs := collectEvents(t, seq, err)
				if len(errs) != 0 {
					t.Fatalf("RunStreamWithHistory errors = %v", errs)
				}
				if calls != 0 {
					t.Errorf("compactor calls = %d, want 0", calls)
				}
				if got := compactionEvents(events); len(got) != 0 {
					t.Errorf("CompactionEvent count = %d, want 0", len(got))
				}
				if len(requests) != 1 || !reflect.DeepEqual(requests[0], current) {
					t.Errorf("provider history = %#v, want unchanged %#v", requests, current)
				}
			})
		}
	})

	t.Run("above threshold compacts before provider rounds", func(t *testing.T) {
		base := []llm.Message{
			llm.SystemMessage("policy"),
			llm.UserMessage("first"),
			llm.AssistantMessage(strings.Repeat("old", 1_000)),
			llm.UserMessage("latest"),
		}
		compactor := CompactFunc(func(_ context.Context, history []llm.Message, _ CompactionBudget) (CompactionResult, error) {
			return CompactionResult{
				History:       []llm.Message{history[0], history[1], history[3], history[4]},
				Strategies:    []string{"test_window"},
				DroppedGroups: 1,
				Changed:       true,
			}, nil
		})
		requests := [][]llm.Message{}
		provider := &messageCapturingMock{
			inner: NewMockProvider(
				MsgResponse(llm.AssistantToolCallMessage(newToolCall("call-1", "mock_tool"))),
				MsgResponse(llm.AssistantMessage("done")),
			),
			onChat: func(messages []llm.Message) {
				requests = append(requests, copyMessages(messages))
			},
		}
		registry := tool.NewRegistry()
		registry.MustRegister(&mockTool{info: tool.ToolInfo{Name: "mock_tool"}, result: tool.NewTextResult("result")})
		agent := New(
			provider,
			registry,
			WithCompactor(compactor),
			WithCompactionThresholdPercent(100),
			WithContextWindowTokens(1_000),
			WithLogger(discardLogger()),
		)

		seq, err := agent.RunStreamWithHistory(context.Background(), base, "current")
		events, errs := collectEvents(t, seq, err)
		if len(errs) != 0 {
			t.Fatalf("RunStreamWithHistory errors = %v", errs)
		}
		if len(compactionEvents(events)) != 1 {
			t.Fatalf("CompactionEvent count = %d, want 1", len(compactionEvents(events)))
		}
		if len(requests) != 2 {
			t.Fatalf("provider calls = %d, want 2", len(requests))
		}
		for round, request := range requests {
			if historyContainsText(request, strings.Repeat("old", 1_000)) {
				t.Errorf("round %d provider request retained compacted history", round+1)
			}
		}
		if !historyContainsText(requests[1], "result") {
			t.Error("round 2 provider request does not contain tool result")
		}
	})

	t.Run("changed compaction emits one accurate event", func(t *testing.T) {
		base := []llm.Message{llm.UserMessage("first"), llm.AssistantMessage(strings.Repeat("old", 500))}
		current := append(copyMessages(base), llm.UserMessage("current"))
		compacted := []llm.Message{current[0], current[2]}
		compactor := CompactFunc(func(_ context.Context, _ []llm.Message, _ CompactionBudget) (CompactionResult, error) {
			return CompactionResult{
				History:       compacted,
				Strategies:    []string{"first", "second"},
				DroppedGroups: 1,
				Changed:       true,
			}, nil
		})
		agent := New(
			NewMockProvider(MsgResponse(llm.AssistantMessage("done"))),
			tool.NewRegistry(),
			WithCompactor(compactor),
			WithCompactionThresholdPercent(100),
			WithContextWindowTokens(1_000),
			WithLogger(discardLogger()),
		)

		seq, err := agent.RunStreamWithHistory(context.Background(), base, "current")
		events, errs := collectEvents(t, seq, err)
		if len(errs) != 0 {
			t.Fatalf("RunStreamWithHistory errors = %v", errs)
		}
		got := compactionEvents(events)
		if len(got) != 1 {
			t.Fatalf("CompactionEvent count = %d, want 1", len(got))
		}
		event := got[0]
		if !reflect.DeepEqual(event.Strategies, []string{"first", "second"}) {
			t.Errorf("Strategies = %#v, want ordered strategies", event.Strategies)
		}
		if event.DroppedGroups != 1 {
			t.Errorf("DroppedGroups = %d, want 1", event.DroppedGroups)
		}
		if event.BeforeRunes != estimateRunes(current) || event.AfterRunes != estimateRunes(compacted) {
			t.Errorf("runes = (%d, %d), want (%d, %d)", event.BeforeRunes, event.AfterRunes, estimateRunes(current), estimateRunes(compacted))
		}
	})

	t.Run("event history deeply isolates mutable block bytes", func(t *testing.T) {
		inputBytes := json.RawMessage(`{"path":"safe"}`)
		image := &llm.ImageBlock{Type: "image", MIMEType: "image/png", Data: []byte("safe-image")}
		assistant := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			llm.ToolUseBlock{Type: "tool_use", ID: "call", Name: "read", Input: inputBytes},
		}}
		base := []llm.Message{
			llm.SystemMessage("policy"),
			llm.UserMessage("first"),
			llm.AssistantMessage(strings.Repeat("old", 500)),
			{Role: llm.RoleUser, Content: []llm.ContentBlock{image}},
			assistant,
			llm.ToolResultMessage("call", tool.NewTextResult("result")),
		}
		compactor := CompactFunc(func(_ context.Context, history []llm.Message, _ CompactionBudget) (CompactionResult, error) {
			return CompactionResult{
				History:    append(copyMessages(history[:2]), history[3:]...),
				Strategies: []string{"deep_copy"},
				Changed:    true,
			}, nil
		})
		requests := [][]llm.Message{}
		provider := &messageCapturingMock{
			inner: NewMockProvider(MsgResponse(llm.AssistantMessage("done"))),
			onChat: func(messages []llm.Message) {
				requests = append(requests, copyMessages(messages))
			},
		}
		agent := New(
			provider,
			tool.NewRegistry(),
			WithCompactor(compactor),
			WithCompactionThresholdPercent(100),
			WithContextWindowTokens(1_000),
			WithLogger(discardLogger()),
		)

		seq, err := agent.RunStreamWithHistory(context.Background(), base, "current")
		if err != nil {
			t.Fatalf("RunStreamWithHistory returned error: %v", err)
		}
		var done DoneEvent
		for event, streamErr := range seq {
			if streamErr != nil {
				t.Fatalf("RunStreamWithHistory emitted error: %v", streamErr)
			}
			switch typed := event.(type) {
			case CompactionEvent:
				for _, message := range typed.History {
					for _, block := range message.Content {
						switch value := block.(type) {
						case llm.ToolUseBlock:
							value.Input[0] = 'X'
						case *llm.ImageBlock:
							value.Data[0] = 'X'
						}
					}
				}
			case DoneEvent:
				done = typed
			}
		}
		if len(requests) != 1 {
			t.Fatalf("provider calls = %d, want 1", len(requests))
		}
		assertMutableBlocksUnchanged(t, requests[0])
		assertMutableBlocksUnchanged(t, done.History)
	})

	t.Run("provider retries reuse one compacted history", func(t *testing.T) {
		base := []llm.Message{llm.UserMessage("first"), llm.AssistantMessage(strings.Repeat("old", 500))}
		calls := 0
		compactor := CompactFunc(func(_ context.Context, history []llm.Message, _ CompactionBudget) (CompactionResult, error) {
			calls++
			return CompactionResult{History: []llm.Message{history[0], history[2]}, Strategies: []string{"once"}, Changed: true}, nil
		})
		attempts := [][]llm.Message{}
		provider := &messageCapturingMock{
			inner: NewRetryableMockProvider(errors.New("connection refused"), 1, llm.AssistantMessage("done")),
			onChat: func(messages []llm.Message) {
				attempts = append(attempts, copyMessages(messages))
			},
		}
		agent := New(
			provider,
			tool.NewRegistry(),
			WithCompactor(compactor),
			WithCompactionThresholdPercent(100),
			WithContextWindowTokens(1_000),
			WithRetryConfig(AgentRetryConfig{MaxRetries: 1, BaseDelay: 1, MaxDelay: 1}),
			WithLogger(discardLogger()),
		)

		seq, err := agent.RunStreamWithHistory(context.Background(), base, "current")
		events, errs := collectEvents(t, seq, err)
		if len(errs) != 0 {
			t.Fatalf("RunStreamWithHistory errors = %v", errs)
		}
		if calls != 1 || len(compactionEvents(events)) != 1 {
			t.Errorf("compactor calls/events = %d/%d, want 1/1", calls, len(compactionEvents(events)))
		}
		if len(attempts) != 2 {
			t.Fatalf("provider attempts = %d, want 2", len(attempts))
		}
		if !reflect.DeepEqual(attempts[0], attempts[1]) {
			t.Errorf("retry histories differ:\nfirst %#v\nsecond %#v", attempts[0], attempts[1])
		}
	})

	t.Run("nil compactor disables compaction", func(t *testing.T) {
		base := []llm.Message{llm.UserMessage(strings.Repeat("large", 1_000))}
		current := append(copyMessages(base), llm.UserMessage("current"))
		requests := [][]llm.Message{}
		provider := &messageCapturingMock{
			inner: NewMockProvider(MsgResponse(llm.AssistantMessage("done"))),
			onChat: func(messages []llm.Message) {
				requests = append(requests, copyMessages(messages))
			},
		}
		agent := New(
			provider,
			tool.NewRegistry(),
			WithCompactor(nil),
			WithContextWindowTokens(100),
			WithLogger(discardLogger()),
		)

		seq, err := agent.RunStreamWithHistory(context.Background(), base, "current")
		events, errs := collectEvents(t, seq, err)
		if len(errs) != 0 {
			t.Fatalf("RunStreamWithHistory errors = %v", errs)
		}
		if len(compactionEvents(events)) != 0 {
			t.Errorf("CompactionEvent count = %d, want 0", len(compactionEvents(events)))
		}
		if len(requests) != 1 || !reflect.DeepEqual(requests[0], current) {
			t.Errorf("provider history = %#v, want original %#v", requests, current)
		}
	})

	t.Run("threshold option and overflow safe budget", func(t *testing.T) {
		base := []llm.Message{llm.UserMessage(strings.Repeat("x", 400))}
		current := append(copyMessages(base), llm.UserMessage("current"))
		window := (estimateRunes(current)*10 + 6) / 7
		for _, tt := range []struct {
			name          string
			percent       int
			expectedCalls int
		}{
			{name: "sixty percent", percent: 60, expectedCalls: 1},
			{name: "eighty percent", percent: 80, expectedCalls: 0},
		} {
			t.Run(tt.name, func(t *testing.T) {
				calls := 0
				compactor := CompactFunc(func(_ context.Context, history []llm.Message, _ CompactionBudget) (CompactionResult, error) {
					calls++
					return CompactionResult{History: []llm.Message{history[len(history)-1]}, Strategies: []string{"threshold"}, Changed: true}, nil
				})
				agent := New(
					NewMockProvider(MsgResponse(llm.AssistantMessage("done"))),
					tool.NewRegistry(),
					WithCompactor(compactor),
					WithCompactionThresholdPercent(tt.percent),
					WithContextWindowTokens(window),
					WithLogger(discardLogger()),
				)
				if _, err := agent.RunWithHistory(context.Background(), base, "current"); err != nil {
					t.Fatalf("RunWithHistory returned error: %v", err)
				}
				if calls != tt.expectedCalls {
					t.Errorf("compactor calls = %d, want %d", calls, tt.expectedCalls)
				}
			})
		}

		calls := 0
		overflowCompactor := CompactFunc(func(_ context.Context, history []llm.Message, _ CompactionBudget) (CompactionResult, error) {
			calls++
			return CompactionResult{History: history}, nil
		})
		agent := New(
			NewMockProvider(MsgResponse(llm.AssistantMessage("done"))),
			tool.NewRegistry(),
			WithCompactor(overflowCompactor),
			WithCompactionThresholdPercent(80),
			WithContextWindowTokens(math.MaxInt),
			WithLogger(discardLogger()),
		)
		if _, err := agent.Run(context.Background(), "small"); err != nil {
			t.Fatalf("Run with MaxInt context returned error: %v", err)
		}
		if calls != 0 {
			t.Errorf("overflow compactor calls = %d, want 0", calls)
		}

		for _, percent := range []int{0, 101} {
			normalized := New(
				NewMockProvider(MsgResponse(llm.AssistantMessage("done"))),
				tool.NewRegistry(),
				WithCompactionThresholdPercent(percent),
			)
			if normalized.compactionThresholdPercent != DefaultCompactionThresholdPercent {
				t.Errorf("threshold %d normalized to %d, want %d", percent, normalized.compactionThresholdPercent, DefaultCompactionThresholdPercent)
			}
		}
	})

	t.Run("standard compactor preserves protected groups", func(t *testing.T) {
		base := []llm.Message{
			llm.SystemMessage("byte-identical-policy"),
			llm.UserMessage("first user"),
			llm.AssistantMessage(strings.Repeat("old answer", 100)),
			llm.UserMessage("middle user"),
			llm.AssistantMessage(strings.Repeat("middle answer", 100)),
		}
		protected := []llm.Message{base[0], base[1], llm.UserMessage("latest user")}
		window := estimateRunes(protected) + 32
		requests := [][]llm.Message{}
		provider := &messageCapturingMock{
			inner: NewMockProvider(MsgResponse(llm.AssistantMessage("done"))),
			onChat: func(messages []llm.Message) {
				requests = append(requests, copyMessages(messages))
			},
		}
		agent := New(
			provider,
			tool.NewRegistry(),
			WithContextWindowTokens(window),
			WithCompactionThresholdPercent(100),
			WithLogger(discardLogger()),
		)

		if _, err := agent.RunWithHistory(context.Background(), base, "latest user"); err != nil {
			t.Fatalf("RunWithHistory returned error: %v", err)
		}
		if len(requests) != 1 {
			t.Fatalf("provider calls = %d, want 1", len(requests))
		}
		request := requests[0]
		if !reflect.DeepEqual(request[0], base[0]) {
			t.Errorf("system prefix changed: %#v, want %#v", request[0], base[0])
		}
		if !historyContainsText(request, "first user") || !historyContainsText(request, "latest user") {
			t.Errorf("protected user groups missing from %#v", request)
		}
		if !reflect.DeepEqual(request[len(request)-1], llm.UserMessage("latest user")) {
			t.Errorf("final group = %#v, want latest user", request[len(request)-1])
		}
	})

	t.Run("uncompactable protected history fails before provider", func(t *testing.T) {
		providerCalls := 0
		provider := &messageCapturingMock{
			inner: NewMockProvider(MsgResponse(llm.AssistantMessage("unused"))),
			onChat: func(_ []llm.Message) {
				providerCalls++
			},
		}
		base := []llm.Message{llm.SystemMessage("policy"), llm.UserMessage(strings.Repeat("protected", 500))}
		agent := New(
			provider,
			tool.NewRegistry(),
			WithContextWindowTokens(100),
			WithCompactionThresholdPercent(100),
			WithLogger(discardLogger()),
		)

		_, err := agent.RunWithHistory(context.Background(), base, "latest")
		if !errors.Is(err, ErrCompactionBudgetExceeded) {
			t.Fatalf("RunWithHistory error = %v, want ErrCompactionBudgetExceeded", err)
		}
		if providerCalls != 0 {
			t.Errorf("provider calls = %d, want 0", providerCalls)
		}
	})

	t.Run("compactor failures warn and preserve original history", func(t *testing.T) {
		compactorErr := errors.New("compactor offline")
		for _, tt := range []struct {
			name      string
			compactor Compactor
			warning   string
		}{
			{
				name: "returned error",
				compactor: CompactFunc(func(_ context.Context, history []llm.Message, _ CompactionBudget) (CompactionResult, error) {
					return CompactionResult{History: history}, compactorErr
				}),
				warning: "history compaction failed",
			},
			{
				name: "invalid system prefix",
				compactor: CompactFunc(func(_ context.Context, history []llm.Message, _ CompactionBudget) (CompactionResult, error) {
					return CompactionResult{History: history[1:], Strategies: []string{"invalid"}, Changed: true}, nil
				}),
				warning: "history compaction produced invalid history",
			},
		} {
			t.Run(tt.name, func(t *testing.T) {
				capture := &truncationLogCapture{}
				requests := [][]llm.Message{}
				provider := &messageCapturingMock{
					inner: NewMockProvider(MsgResponse(llm.AssistantMessage("done"))),
					onChat: func(messages []llm.Message) {
						requests = append(requests, copyMessages(messages))
					},
				}
				base := []llm.Message{llm.SystemMessage("policy"), llm.UserMessage(strings.Repeat("large", 500))}
				current := append(copyMessages(base), llm.UserMessage("current"))
				agent := New(
					provider,
					tool.NewRegistry(),
					WithCompactor(tt.compactor),
					WithContextWindowTokens(100),
					WithCompactionThresholdPercent(100),
					WithLogger(newCompactionLogger(capture)),
				)

				if _, err := agent.RunWithHistory(context.Background(), base, "current"); err != nil {
					t.Fatalf("RunWithHistory returned error: %v", err)
				}
				if len(requests) != 1 || !reflect.DeepEqual(requests[0], current) {
					t.Errorf("provider history = %#v, want original %#v", requests, current)
				}
				if len(capture.recordsWithMessage(tt.warning)) != 1 {
					t.Errorf("warning %q count = %d, want 1", tt.warning, len(capture.recordsWithMessage(tt.warning)))
				}
			})
		}
	})

	t.Run("standard compactor supports manual use", func(t *testing.T) {
		history := []llm.Message{
			llm.SystemMessage("policy"),
			llm.UserMessage("first"),
			llm.AssistantMessage(strings.Repeat("old", 500)),
			llm.UserMessage("latest"),
		}
		expected := []llm.Message{history[0], history[1], history[3]}
		result, err := NewStandardCompactor().Compact(
			context.Background(),
			history,
			CompactionBudget{MaxRunes: estimateRunes(expected)},
		)
		if err != nil {
			t.Fatalf("Compact returned error: %v", err)
		}
		if !result.Changed || !reflect.DeepEqual(result.History, expected) {
			t.Errorf("Compact result = %#v, want %#v", result, expected)
		}
	})

	t.Run("run and runstream remain equivalent", func(t *testing.T) {
		newAgent := func() *Agent {
			compactor := CompactFunc(func(_ context.Context, _ []llm.Message, _ CompactionBudget) (CompactionResult, error) {
				return CompactionResult{History: []llm.Message{llm.UserMessage("compacted")}, Strategies: []string{"equivalence"}, DroppedGroups: 1, Changed: true}, nil
			})
			return New(
				NewMockProvider(MsgResponse(llm.AssistantMessage("done"))),
				tool.NewRegistry(),
				WithCompactor(compactor),
				WithCompactionThresholdPercent(100),
				WithContextWindowTokens(1_000),
				WithLogger(discardLogger()),
			)
		}
		input := strings.Repeat("oversized", 500)

		runResult, err := newAgent().Run(context.Background(), input)
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
		seq, err := newAgent().RunStream(context.Background(), input)
		if err != nil {
			t.Fatalf("RunStream returned error: %v", err)
		}
		var done DoneEvent
		for event, streamErr := range seq {
			if streamErr != nil {
				t.Fatalf("RunStream emitted error: %v", streamErr)
			}
			if value, ok := event.(DoneEvent); ok {
				done = value
			}
		}
		streamResult := &RunResult{
			Message: done.Message, History: done.History, ToolCalls: done.ToolCalls,
			Truncated: done.Truncated, Retries: done.Retries, Usage: done.Usage, TotalUsage: done.TotalUsage,
		}
		if !reflect.DeepEqual(streamResult, runResult) {
			t.Errorf("RunStream result = %#v, want Run result %#v", streamResult, runResult)
		}
	})

	t.Run("summarization chain integrates success failure and cancellation", func(t *testing.T) {
		base := []llm.Message{
			llm.SystemMessage("policy"),
			llm.UserMessage("first"),
			llm.AssistantMessage(strings.Repeat("old answer", 100)),
			llm.UserMessage("middle"),
			llm.AssistantMessage(strings.Repeat("more history", 100)),
		}
		protected := []llm.Message{base[0], base[1], llm.UserMessage("latest")}
		budget := estimateRunes(append(copyMessages(protected[:2]), llm.AssistantMessage("[Summary]\nsummary text"), protected[2])) + 32

		t.Run("success inserts summary", func(t *testing.T) {
			summarizer := &compactionSummaryProvider{message: compactorMessagePointer(llm.AssistantMessage("summary text"))}
			requests := [][]llm.Message{}
			provider := &messageCapturingMock{
				inner: NewMockProvider(MsgResponse(llm.AssistantMessage("done"))),
				onChat: func(messages []llm.Message) {
					requests = append(requests, copyMessages(messages))
				},
			}
			agent := New(
				provider,
				tool.NewRegistry(),
				WithCompactor(NewCompactorChain(
					NewDropOldestToolGroupsCompactor(),
					NewSummarizationCompactor(summarizer),
					NewSlidingWindowCompactor(),
				)),
				WithContextWindowTokens(budget),
				WithCompactionThresholdPercent(100),
				WithLogger(discardLogger()),
			)
			if _, err := agent.RunWithHistory(context.Background(), base, "latest"); err != nil {
				t.Fatalf("RunWithHistory returned error: %v", err)
			}
			if summarizer.calls != 1 || len(requests) != 1 || !historyContainsText(requests[0], "[Summary]\nsummary text") {
				t.Errorf("summary calls/history = %d/%#v", summarizer.calls, requests)
			}
		})

		t.Run("failure degrades to window and logs error", func(t *testing.T) {
			capture := &truncationLogCapture{}
			summarizerErr := errors.New("summarizer failed")
			summarizer := &compactionSummaryProvider{err: summarizerErr}
			chain := NewCompactorChain(
				NewDropOldestToolGroupsCompactor(),
				NewSummarizationCompactor(summarizer),
				NewSlidingWindowCompactor(),
			)
			current := append(copyMessages(base), llm.UserMessage("latest"))
			manualResult, err := chain.Compact(
				context.Background(),
				current,
				CompactionBudget{MaxRunes: estimateRunes(protected) + 8},
			)
			if err != nil {
				t.Fatalf("chain Compact returned error: %v", err)
			}
			if len(manualResult.Errors) == 0 || !manualResult.Changed {
				t.Fatalf("degraded chain result = %#v, want errors and window change", manualResult)
			}
			requests := [][]llm.Message{}
			provider := &messageCapturingMock{
				inner: NewMockProvider(MsgResponse(llm.AssistantMessage("done"))),
				onChat: func(messages []llm.Message) {
					requests = append(requests, copyMessages(messages))
				},
			}
			agent := New(
				provider,
				tool.NewRegistry(),
				WithCompactor(chain),
				WithContextWindowTokens(estimateRunes(protected)+8),
				WithCompactionThresholdPercent(100),
				WithLogger(newCompactionLogger(capture)),
			)
			seq, err := agent.RunStreamWithHistory(context.Background(), base, "latest")
			events, errs := collectEvents(t, seq, err)
			if len(errs) != 0 {
				t.Fatalf("RunStreamWithHistory errors = %v", errs)
			}
			compactions := compactionEvents(events)
			if len(compactions) != 1 || len(requests) != 1 {
				t.Fatalf("compactions/provider calls = %d/%d, want 1/1", len(compactions), len(requests))
			}
			if len(compactions[0].Strategies) != 3 || historyContainsText(requests[0], "old answer") {
				t.Errorf("window fallback did not compact: event=%#v history=%#v", compactions[0], requests[0])
			}
			if len(capture.recordsWithMessage("history compaction strategy failed")) == 0 {
				t.Error("missing degraded compaction warning")
			}
		})

		t.Run("cancellation during summarization ends run", func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			summarizer := &compactionSummaryProvider{errFunc: func() error {
				cancel()
				return ctx.Err()
			}}
			providerCalls := 0
			provider := &messageCapturingMock{
				inner: NewMockProvider(MsgResponse(llm.AssistantMessage("unused"))),
				onChat: func(_ []llm.Message) {
					providerCalls++
				},
			}
			agent := New(
				provider,
				tool.NewRegistry(),
				WithCompactor(NewCompactorChain(
					NewDropOldestToolGroupsCompactor(),
					NewSummarizationCompactor(summarizer),
					NewSlidingWindowCompactor(),
				)),
				WithContextWindowTokens(estimateRunes(protected)+8),
				WithCompactionThresholdPercent(100),
				WithLogger(discardLogger()),
			)
			_, err := agent.RunWithHistory(ctx, base, "latest")
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("RunWithHistory error = %v, want context.Canceled", err)
			}
			if providerCalls != 0 {
				t.Errorf("main provider calls = %d, want 0", providerCalls)
			}
		})
	})
}

func compactionEvents(events []AgentEvent) []CompactionEvent {
	result := []CompactionEvent{}
	for _, event := range events {
		if compacted, ok := event.(CompactionEvent); ok {
			result = append(result, compacted)
		}
	}
	return result
}

func historyContainsText(history []llm.Message, text string) bool {
	for _, message := range history {
		for _, block := range message.Content {
			switch value := block.(type) {
			case llm.TextBlock:
				if strings.Contains(value.Text, text) {
					return true
				}
			case *llm.TextBlock:
				if value != nil && strings.Contains(value.Text, text) {
					return true
				}
			case llm.ToolResultBlock:
				if strings.Contains(value.Content, text) {
					return true
				}
			case *llm.ToolResultBlock:
				if value != nil && strings.Contains(value.Content, text) {
					return true
				}
			}
		}
	}
	return false
}

func assertMutableBlocksUnchanged(t *testing.T, history []llm.Message) {
	t.Helper()
	foundTool := false
	foundImage := false
	for _, message := range history {
		for _, block := range message.Content {
			switch value := block.(type) {
			case llm.ToolUseBlock:
				foundTool = true
				if string(value.Input) != `{"path":"safe"}` {
					t.Errorf("tool input = %q, want unchanged", value.Input)
				}
			case *llm.ImageBlock:
				foundImage = true
				if string(value.Data) != "safe-image" {
					t.Errorf("image data = %q, want unchanged", value.Data)
				}
			}
		}
	}
	if !foundTool || !foundImage {
		t.Errorf("mutable blocks found = tool:%t image:%t, want both", foundTool, foundImage)
	}
}

func newCompactionLogger(capture *truncationLogCapture) *slog.Logger {
	return slog.New(capture)
}

type compactionSummaryProvider struct {
	message *llm.Message
	err     error
	errFunc func() error
	calls   int
}

func (p *compactionSummaryProvider) Name() string { return "compaction-summary" }

func (p *compactionSummaryProvider) Chat(
	_ context.Context,
	_ []llm.Message,
	_ []tool.ToolInfo,
	_ ...llm.Option,
) (*llm.Message, *llm.Usage, error) {
	p.calls++
	if p.errFunc != nil {
		return nil, nil, p.errFunc()
	}
	return p.message, nil, p.err
}

func (p *compactionSummaryProvider) ChatStream(
	_ context.Context,
	_ []llm.Message,
	_ []tool.ToolInfo,
	_ ...llm.Option,
) (iter.Seq2[llm.Chunk, error], error) {
	return nil, fmt.Errorf("unused")
}
