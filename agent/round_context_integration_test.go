package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

func TestRoundContextCompactionAndCancellation(t *testing.T) {
	t.Parallel()

	t.Run("same round compacts canonical history before adding overlay", func(t *testing.T) {
		old := strings.Repeat("old", 300)
		var compactedInput []llm.Message
		compactor := CompactFunc(func(_ context.Context, history []llm.Message, _ CompactionBudget) (CompactionResult, error) {
			compactedInput = deepCopyMessages(history)
			return CompactionResult{History: []llm.Message{history[len(history)-1]}, Changed: true}, nil
		})
		p := &capturedProvider{streams: []capturedStreamResult{{chunks: []llm.Chunk{llm.DoneChunk{FinishReason: "stop"}}}}}
		ag := New(p, tool.NewRegistry(),
			WithCompactor(compactor),
			WithCompactionThresholdPercent(100),
			WithContextWindowTokens(1_000),
			WithRoundContextProvider(func(context.Context, RoundContextRequest) (string, error) {
				return "compaction-snapshot", nil
			}),
			WithLogger(discardLogger()),
		)
		seq, err := ag.RunStreamWithHistory(context.Background(), []llm.Message{
			llm.UserMessage("first"), llm.AssistantMessage(old),
		}, "current")
		events, errs := collectEvents(t, seq, err)
		if len(errs) != 0 || len(compactedInput) == 0 || historyContainsText(compactedInput, "compaction-snapshot") {
			t.Fatalf("compaction errors/history = %v, %#v", errs, compactedInput)
		}
		if len(p.calls) != 1 || !strings.Contains(capturedFinalText(p.calls[0]), "compaction-snapshot") ||
			historyContainsText(p.calls[0].history, old) {
			t.Fatalf("outbound history did not use compacted canonical history plus overlay")
		}
		for _, event := range events {
			if compacted, ok := event.(CompactionEvent); ok && historyContainsText(compacted.History, "compaction-snapshot") {
				t.Fatal("CompactionEvent retained the transient overlay")
			}
		}
	})

	t.Run("cancellation after callback preparation becomes RoundContextError", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		p := &capturedProvider{}
		ag := New(p, tool.NewRegistry(), WithRoundContextProvider(func(context.Context, RoundContextRequest) (string, error) {
			cancel()
			return "snapshot", nil
		}), WithLogger(discardLogger()))
		_, err := ag.Run(ctx, "input")
		var contextErr *RoundContextError
		if !errors.As(err, &contextErr) {
			t.Fatalf("Run() error = %v, want RoundContextError", err)
		}
		if !errors.Is(err, context.Canceled) || contextErr.Attempt != 1 || len(p.calls) != 0 {
			t.Errorf("Run() error, attempt, provider calls = %v, %d, %d; want canceled RoundContextError, 1, 0", err, contextErr.Attempt, len(p.calls))
		}
	})
}

type roundContextCountingStore struct {
	store.Store
	runStarts int
}

func (s *roundContextCountingStore) Append(ctx context.Context, thread string, expected int64, records ...store.Record) (int64, error) {
	for _, record := range records {
		if record.Kind == store.KindRunStarted {
			s.runStarts++
		}
	}
	return s.Store.Append(ctx, thread, expected, records...)
}

func TestRunInterruptedErrorPreventsGeneratedThreadRebuild(t *testing.T) {
	st := &roundContextCountingStore{Store: store.NewMemory()}
	p := &capturedProvider{streams: []capturedStreamResult{{err: ErrThreadBusy}}}
	ag := New(p, tool.NewRegistry(), WithStore(st), WithLogger(discardLogger()))
	_, err := ag.Run(context.Background(), "input")
	var interrupted *RunInterruptedError
	if !errors.Is(err, ErrThreadBusy) || !errors.As(err, &interrupted) || interrupted.ThreadID == "" {
		t.Fatalf("Run() error = %v, want wrapped ErrThreadBusy with its thread", err)
	}
	if st.runStarts != 1 {
		t.Errorf("run_started writes = %d, want 1 without generated-thread rebuild", st.runStarts)
	}
}
