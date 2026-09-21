package agent

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

type settleInterruptCommitStore struct {
	store.Store
	cancel context.CancelFunc
}

func (s *settleInterruptCommitStore) Append(
	ctx context.Context, thread string, head int64, records ...store.Record,
) (int64, error) {
	next, err := s.Store.Append(ctx, thread, head, records...)
	if err == nil && len(records) == 1 && records[0].Kind == store.KindRoundCommitted {
		s.cancel()
	}
	return next, err
}

func TestSettleInterruptCommittedResults(t *testing.T) {
	for _, tc := range []struct {
		name        string
		concurrency int
	}{
		{name: "E05_committed_before_next_provider", concurrency: 1},
		{name: "E07_parallel_slots_resolved_before_cancel", concurrency: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settleInterruptStores(t, func(t *testing.T, backing store.Store) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				var st store.Store = backing
				if tc.concurrency == 1 {
					st = &settleInterruptCommitStore{Store: backing, cancel: cancel}
				}
				reg := tool.NewRegistry()
				workers := make([]*parallelTestTool, 0, 2)
				for _, name := range []string{"success", "soft_error"} {
					worker := &parallelTestTool{info: tool.ToolInfo{Name: name},
						execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
							if name == "soft_error" {
								return &tool.ToolResult{Status: tool.ResultError, Content: "real rejection"}, nil
							}
							return tool.NewTextResult("real success"), nil
						},
					}
					workers = append(workers, worker)
					reg.MustRegister(worker)
				}
				p := parallelProvider(parallelToolCalls("success", "soft_error"))
				a := New(p, reg, WithStore(st), WithToolConcurrency(tc.concurrency), WithLogger(discardLogger()))
				seq, err := a.RunThreadStream(ctx, "t", "old")
				if err != nil {
					t.Fatal(err)
				}
				live := 0
				for event, streamErr := range seq {
					if _, ok := event.(ToolResultEvent); ok {
						live++
						if live == 2 && tc.concurrency == 2 {
							cancel()
						}
					}
					if streamErr != nil {
						err = streamErr
					}
				}
				token := settleInterruptToken(t, err, context.Canceled)
				before := logRecords(t, st, "t")
				equalKinds(t, logKinds(t, st, "t"),
					store.KindRunStarted, store.KindRoundDeclared, store.KindRoundCommitted)
				if live != 2 || p.index != 1 {
					t.Fatalf("live results=%d provider calls=%d", live, p.index)
				}
				snapshot := settleInterruptSnapshot(t, a, token)
				after := logRecords(t, st, "t")
				if !reflect.DeepEqual(before, after[:len(before)]) {
					t.Fatal("settlement rewrote the real committed prefix")
				}
				settleInterruptEmptyCancel(t, st)
				got := historyResultBlocks(snapshot.History)
				want := []llm.ToolResultBlock{
					{Type: "tool_result", ToolUseID: "c1", Content: "real success"},
					{Type: "tool_result", ToolUseID: "c2", Content: "real rejection", IsError: true},
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("committed results = %+v, want %+v", got, want)
				}
				if p.index != 1 || workers[0].calls.Load() != 1 || workers[1].calls.Load() != 1 {
					t.Fatal("settlement repeated provider or tool execution")
				}
			})
		})
	}
}
