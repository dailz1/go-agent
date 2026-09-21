package agent

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

func settleInterruptStores(t *testing.T, test func(*testing.T, store.Store)) {
	t.Helper()
	for _, backend := range []string{"memory", "jsonl"} {
		t.Run(backend, func(t *testing.T) {
			var st store.Store = store.NewMemory()
			if backend == "jsonl" {
				disk, err := store.NewJSONL(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := disk.Close(); err != nil {
						t.Error(err)
					}
				})
				st = disk
			}
			test(t, st)
		})
	}
}

func settleInterruptAwait[T any](t *testing.T, signal <-chan T) T {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	select {
	case value := <-signal:
		return value
	case <-ctx.Done():
		t.Fatal("interruption gate did not complete")
		var zero T
		return zero
	}
}

type settleInterruptProvider struct {
	*MockStreamingProvider
	run      func(context.Context, []llm.Message, func(llm.Chunk, error) bool)
	calls    atomic.Int32
	requests [][]llm.Message
}

func (p *settleInterruptProvider) ChatStream(
	ctx context.Context, messages []llm.Message, _ []tool.ToolInfo, _ ...llm.Option,
) (iter.Seq2[llm.Chunk, error], error) {
	p.calls.Add(1)
	p.requests = append(p.requests, deepCopyMessages(messages))
	return func(yield func(llm.Chunk, error) bool) { p.run(ctx, messages, yield) }, nil
}

func settleInterruptToken(t *testing.T, err error, cause error) SettlementToken {
	t.Helper()
	var interrupted *RunInterruptedError
	if !errors.Is(err, cause) || !errors.As(err, &interrupted) || interrupted.Settlement == nil {
		t.Fatalf("interruption = %v, want wrapper and settlement token for %v", err, cause)
	}
	token := *interrupted.Settlement
	if token.ThreadID != "t" || token.RunID == "" || token.ExpectedHead < 1 {
		t.Fatalf("invalid exit token: %+v", token)
	}
	return token
}

func settleInterruptSnapshot(t *testing.T, a *Agent, token SettlementToken) *RunResult {
	t.Helper()
	if err := a.SettleThread(t.Context(), token); err != nil {
		t.Fatalf("SettleThread: %v", err)
	}
	result, err := a.ResumeThread(t.Context(), token.ThreadID)
	if err != nil || result == nil || !result.Cancelled {
		t.Fatalf("cancelled snapshot = %+v, error = %v", result, err)
	}
	if _, err := partitionHistory(result.History); err != nil {
		t.Fatalf("cancelled history is unpaired: %v", err)
	}
	return result
}

func settleInterruptUnknown(t *testing.T, history []llm.Message, ids ...string) {
	t.Helper()
	results := historyResultBlocks(history)
	if len(results) != len(ids) {
		t.Fatalf("results = %+v, want %d unknowns", results, len(ids))
	}
	for i, result := range results {
		if result.ToolUseID != ids[i] || !result.IsError || !strings.Contains(result.Content, "outcome unknown") {
			t.Errorf("result %d = %+v, want unknown for %s", i, result, ids[i])
		}
	}
	for _, message := range history {
		if message.Role == llm.RoleTool && len(message.Content) != 1 {
			t.Errorf("tool result has extra content: %+v", message)
		}
	}
}

type settleInterruptAppendStore struct {
	store.Store
	kind       string
	afterWrite bool
	err        error
	attempts   []store.Record
	revisions  []int64
}

func (s *settleInterruptAppendStore) Append(
	ctx context.Context, thread string, head int64, records ...store.Record,
) (int64, error) {
	if len(records) != 1 || records[0].Kind != s.kind {
		return s.Store.Append(ctx, thread, head, records...)
	}
	s.attempts = append(s.attempts, records[0])
	s.revisions = append(s.revisions, head)
	if len(s.attempts) != 1 {
		return s.Store.Append(ctx, thread, head, records...)
	}
	if s.afterWrite {
		if _, err := s.Store.Append(ctx, thread, head, records...); err != nil {
			return 0, err
		}
	}
	return 0, s.err
}

func settleInterruptEmptyCancel(t *testing.T, st store.Store) {
	t.Helper()
	records := logRecords(t, st, "t")
	last := records[len(records)-1]
	var payload struct {
		RunID     string        `json:"run_id"`
		OpenRound *int          `json:"open_round"`
		Results   []llm.Message `json:"results"`
	}
	if err := json.Unmarshal(last.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if last.Kind != "run_cancelled" || payload.OpenRound != nil || len(payload.Results) != 0 {
		t.Fatalf("cancel after committed round = %s", last.Payload)
	}
}
