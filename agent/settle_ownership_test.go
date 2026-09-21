package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
)

func settleAwait[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("settlement synchronization timed out")
		var zero T
		return zero
	}
}

type settleGateStore struct {
	store.Store
	kind    string
	entered chan struct{}
	release chan struct{}
}

func (s *settleGateStore) Append(ctx context.Context, thread string, head int64, records ...store.Record) (int64, error) {
	if len(records) > 0 && records[0].Kind == s.kind {
		s.entered <- struct{}{}
		select {
		case <-s.release:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	return s.Store.Append(ctx, thread, head, records...)
}

func TestSettleOwnership(t *testing.T) {
	for _, sharedAgent := range []bool{false, true} {
		t.Run(map[bool]string{false: "same_agent", true: "E46_shared_store"}[sharedAgent], func(t *testing.T) {
			ctx := context.Background()
			st := &settleGateStore{Store: store.NewMemory(), kind: store.KindRunCancelled,
				entered: make(chan struct{}, 1), release: make(chan struct{})}
			release := sync.OnceFunc(func() { close(st.release) })
			defer release()
			head := stagedRunStarted(t, st, "t", "r", "old")
			ag := persistTestAgent(t, st)
			competitor := ag
			if sharedAgent {
				competitor = persistTestAgent(t, st)
			}
			target := SettlementToken{ThreadID: "t", RunID: "r", ExpectedHead: head}
			done := make(chan error, 1)
			go func() { done <- ag.SettleThread(ctx, target) }()
			settleAwait(t, st.entered)
			for _, test := range []struct {
				name string
				call func() error
			}{
				{"E25_settle", func() error { return competitor.SettleThread(ctx, target) }},
				{"E26_run", func() error { _, err := competitor.RunThread(ctx, "t", "new"); return err }},
				{"E27_resume", func() error { _, err := competitor.ResumeThread(ctx, "t"); return err }},
				{"inspection", func() error { _, err := competitor.SettlementTarget(ctx, "t"); return err }},
			} {
				t.Run(test.name, func(t *testing.T) {
					if err := test.call(); !errors.Is(err, ErrThreadBusy) {
						t.Fatalf("concurrent operation = %v", err)
					}
				})
			}
			release()
			if err := settleAwait(t, done); err != nil {
				t.Fatal(err)
			}
			if err := competitor.SettleThread(ctx, target); err != nil {
				t.Fatal(err)
			}
			equalKinds(t, logKinds(t, st, "t"), store.KindRunStarted, store.KindRunCancelled)
			res, err := competitor.ResumeThread(ctx, "t")
			if err != nil || !res.Cancelled {
				t.Fatalf("snapshot = %#v, %v", res, err)
			}
		})
	}
}

func TestSettleExecutorOwnership(t *testing.T) {
	for _, resume := range []bool{false, true} {
		t.Run(map[bool]string{false: "E26_run_first", true: "E27_resume_first"}[resume], func(t *testing.T) {
			ctx := context.Background()
			st := store.NewMemory()
			head := stagedRunStarted(t, st, "t", "r", "old")
			target := SettlementToken{ThreadID: "t", RunID: "r", ExpectedHead: head}
			gate := &gateProvider{entered: make(chan struct{}, 1), release: make(chan struct{}), reply: llm.AssistantMessage("done")}
			release := sync.OnceFunc(func() { close(gate.release) })
			defer release()
			ag := persistTestAgent(t, st)
			ag.provider = gate
			if !resume {
				if _, err := ag.RunThread(ctx, "t", "new"); !errors.Is(err, ErrRunIncomplete) {
					t.Fatalf("run on incomplete = %v", err)
				}
				if err := ag.SettleThread(ctx, target); err != nil {
					t.Fatal(err)
				}
			}
			done := make(chan error, 1)
			go func() {
				var err error
				if resume {
					_, err = ag.ResumeThread(ctx, "t")
				} else {
					_, err = ag.RunThread(ctx, "t", "new")
				}
				done <- err
			}()
			settleAwait(t, gate.entered)
			competitor := persistTestAgent(t, st)
			if err := competitor.SettleThread(ctx, target); !errors.Is(err, ErrThreadBusy) {
				t.Fatalf("settle running executor = %v", err)
			}
			release()
			if err := settleAwait(t, done); err != nil {
				t.Fatal(err)
			}
		})
	}
}
