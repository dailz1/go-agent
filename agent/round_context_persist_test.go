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

func TestRoundContextProviderPersistence(t *testing.T) {
	t.Parallel()

	t.Run("store replay excludes transient snapshots and resume recollects", func(t *testing.T) {
		st := store.NewMemory()
		registry := tool.NewRegistry()
		registry.MustRegister(&mockTool{info: tool.ToolInfo{Name: "echo"}, result: tool.NewTextResult("ok")})
		p := &capturedProvider{streams: []capturedStreamResult{
			{chunks: []llm.Chunk{
				llm.ToolCallStartChunk{Index: 0, ID: "call", Name: "echo"},
				llm.ToolCallArgsChunk{Index: 0, Delta: `{}`},
				llm.DoneChunk{FinishReason: "tool_calls"},
			}},
			{chunks: []llm.Chunk{llm.DoneChunk{FinishReason: "stop"}}},
		}}
		var requests []RoundContextRequest
		ag := New(p, registry, WithStore(st), WithRoundContextProvider(func(_ context.Context, request RoundContextRequest) (string, error) {
			requests = append(requests, request)
			if len(requests) == 1 {
				return "snapshot-A", nil
			}
			return "snapshot-B", nil
		}), WithLogger(discardLogger()))

		seq, err := ag.RunThreadStream(context.Background(), "thread", "input")
		if err != nil {
			t.Fatalf("RunThreadStream() error = %v", err)
		}
		for event, iterErr := range seq {
			if iterErr != nil {
				t.Fatalf("initial stream error = %v", iterErr)
			}
			if _, ok := event.(ToolCallEvent); ok {
				break
			}
		}
		if _, err := ag.ResumeThread(context.Background(), "thread"); err != nil {
			t.Fatalf("ResumeThread() error = %v", err)
		}
		if len(requests) != 2 || requests[0].Round != 0 || requests[1].Round != 1 {
			t.Fatalf("callback requests = %#v, want rounds 0 then 1", requests)
		}
		if len(p.calls) != 2 || !strings.Contains(capturedFinalText(p.calls[0]), "snapshot-A") ||
			strings.Contains(capturedFinalText(p.calls[1]), "snapshot-A") ||
			!strings.Contains(capturedFinalText(p.calls[1]), "snapshot-B") {
			t.Fatalf("outbound snapshots = %q, %q; want fresh A then only B",
				capturedFinalText(p.calls[0]), capturedFinalText(p.calls[1]))
		}
		for _, record := range logRecords(t, st, "thread") {
			if strings.Contains(string(record.Payload), "snapshot-") {
				t.Fatalf("store record leaked snapshot: %s", record.Payload)
			}
		}
	})

	t.Run("RunWithHistory preserves caller history and excludes overlay", func(t *testing.T) {
		history := []llm.Message{llm.SystemMessage("system"), llm.UserMessage("old")}
		p := &capturedProvider{streams: []capturedStreamResult{{chunks: []llm.Chunk{llm.DoneChunk{FinishReason: "stop"}}}}}
		ag := New(p, tool.NewRegistry(), WithRoundContextProvider(func(context.Context, RoundContextRequest) (string, error) {
			return "history-snapshot", nil
		}), WithLogger(discardLogger()))
		result, err := ag.RunWithHistory(context.Background(), history, "new")
		if err != nil {
			t.Fatalf("RunWithHistory() error = %v", err)
		}
		if strings.Contains(messageText(&history[0]), "history-snapshot") ||
			!strings.Contains(capturedFinalText(p.calls[0]), "history-snapshot") {
			t.Fatal("overlay did not remain outbound-only")
		}
		for _, message := range result.History {
			if strings.Contains(messageText(&message), "history-snapshot") {
				t.Fatal("RunWithHistory result retained the overlay")
			}
		}
	})
}

func TestRunInterruptedErrorPersistentBoundaries(t *testing.T) {
	t.Parallel()

	t.Run("post-session callback error is resumable", func(t *testing.T) {
		st := store.NewMemory()
		p := &capturedProvider{streams: []capturedStreamResult{{chunks: []llm.Chunk{llm.DoneChunk{FinishReason: "stop"}}}}}
		want := errors.New("snapshot unavailable")
		attempt := 0
		ag := New(p, tool.NewRegistry(), WithStore(st), WithRoundContextProvider(func(context.Context, RoundContextRequest) (string, error) {
			attempt++
			if attempt == 1 {
				return "", want
			}
			return "fresh", nil
		}), WithLogger(discardLogger()))
		_, err := ag.Run(context.Background(), "input")
		var interrupted *RunInterruptedError
		if !errors.As(err, &interrupted) || interrupted.ThreadID == "" || !errors.Is(err, want) {
			t.Fatalf("Run() error = %v, want resumable wrapper for %v", err, want)
		}
		if _, err := ag.ResumeThread(context.Background(), interrupted.ThreadID); err != nil {
			t.Fatalf("ResumeThread(%q) error = %v", interrupted.ThreadID, err)
		}
	})

	t.Run("pre-session cancellation is not wrapped", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		ag := New(&capturedProvider{}, tool.NewRegistry(), WithStore(store.NewMemory()), WithLogger(discardLogger()))
		_, err := ag.RunThread(ctx, "thread", "input")
		var interrupted *RunInterruptedError
		if !errors.Is(err, context.Canceled) || errors.As(err, &interrupted) {
			t.Errorf("RunThread() error = %v, want unwrapped context cancellation", err)
		}
	})

	t.Run("early stream break reports no synthetic interruption", func(t *testing.T) {
		st := store.NewMemory()
		p := &capturedProvider{streams: []capturedStreamResult{
			{chunks: []llm.Chunk{llm.TextDeltaChunk{Text: "partial"}}},
			{chunks: []llm.Chunk{llm.DoneChunk{FinishReason: "stop"}}},
		}}
		ag := New(p, tool.NewRegistry(), WithStore(st), WithLogger(discardLogger()))
		seq, err := ag.RunThreadStream(context.Background(), "thread", "input")
		if err != nil {
			t.Fatalf("RunThreadStream() error = %v", err)
		}
		for event, iterErr := range seq {
			if iterErr != nil {
				t.Fatalf("unexpected stream error = %v", iterErr)
			}
			if _, ok := event.(TextDeltaEvent); ok {
				break
			}
		}
		if _, err := ag.ResumeThread(context.Background(), "thread"); err != nil {
			t.Fatalf("ResumeThread() after early break error = %v", err)
		}
	})

	t.Run("post-session budget failure exposes thread and sentinel", func(t *testing.T) {
		ag := New(&capturedProvider{}, tool.NewRegistry(),
			WithStore(store.NewMemory()),
			WithContextWindowTokens(1),
			WithRoundContextProvider(func(context.Context, RoundContextRequest) (string, error) {
				return "unused", nil
			}),
			WithLogger(discardLogger()),
		)
		_, err := ag.RunThread(context.Background(), "budget-thread", "input")
		var interrupted *RunInterruptedError
		if !errors.As(err, &interrupted) || interrupted.ThreadID != "budget-thread" ||
			!errors.Is(err, ErrCompactionBudgetExceeded) {
			t.Errorf("RunThread() error = %v, want budget-thread RunInterruptedError wrapping budget sentinel", err)
		}
	})

	t.Run("post-session busy failure exposes thread and sentinel", func(t *testing.T) {
		p := &capturedProvider{streams: []capturedStreamResult{{err: ErrThreadBusy}}}
		ag := New(p, tool.NewRegistry(), WithStore(store.NewMemory()), WithLogger(discardLogger()))
		_, err := ag.RunThread(context.Background(), "busy-thread", "input")
		var interrupted *RunInterruptedError
		if !errors.As(err, &interrupted) || interrupted.ThreadID != "busy-thread" || !errors.Is(err, ErrThreadBusy) {
			t.Errorf("RunThread() error = %v, want busy-thread RunInterruptedError wrapping ErrThreadBusy", err)
		}
	})

	t.Run("closed-thread sentinel penetrates interruption wrapper", func(t *testing.T) {
		p := &capturedProvider{streams: []capturedStreamResult{{chunks: []llm.Chunk{
			llm.DoneChunk{FinishReason: "stop"},
		}}}}
		ag := New(p, tool.NewRegistry(), WithStore(store.NewMemory()), WithLogger(discardLogger()))
		if _, err := ag.RunThread(context.Background(), "closed-thread", "input"); err != nil {
			t.Fatalf("RunThread() error = %v", err)
		}
		_, closedErr := ag.ResumeThread(context.Background(), "closed-thread")
		if !errors.Is(closedErr, ErrNothingToResume) {
			t.Fatalf("ResumeThread() error = %v, want ErrNothingToResume", closedErr)
		}
		err := &RunInterruptedError{ThreadID: "closed-thread", Err: closedErr}
		var interrupted *RunInterruptedError
		if !errors.As(err, &interrupted) || interrupted.ThreadID != "closed-thread" || !errors.Is(err, ErrNothingToResume) {
			t.Errorf("interruption error = %v, want closed-thread wrapper exposing ErrNothingToResume", err)
		}
	})
}
