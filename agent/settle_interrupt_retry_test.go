package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

func TestSettleInterruptRetry(t *testing.T) {
	for _, tc := range []struct {
		name       string
		fail       bool
		afterWrite bool
	}{
		{name: "E14_double_settle"},
		{name: "E15_ack_lost", fail: true, afterWrite: true},
		{name: "E16_before_write_failure", fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settleInterruptStores(t, func(t *testing.T, backing store.Store) {
				cause := errors.New("cancel append failed")
				fault := &settleInterruptAppendStore{
					Store: backing, kind: "run_cancelled", afterWrite: tc.afterWrite, err: cause,
				}
				var st store.Store = backing
				if tc.fail {
					st = fault
				}
				reg := tool.NewRegistry()
				worker := &parallelTestTool{info: tool.ToolInfo{Name: "one"},
					execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
						return tool.NewTextResult("must not execute"), nil
					},
				}
				reg.MustRegister(worker)
				exit := make(chan SettlementToken, 1)
				p := parallelProvider(parallelToolCalls("one", "one"))
				a := New(p, reg, WithStore(st), WithLogger(discardLogger()),
					WithRunExitFn(func(token SettlementToken) { exit <- token }))
				seq, err := a.RunThreadStream(t.Context(), "t", "old")
				if err != nil {
					t.Fatal(err)
				}
				for event, streamErr := range seq {
					if streamErr != nil {
						t.Fatal(streamErr)
					}
					if _, ok := event.(ToolCallEvent); ok {
						break
					}
				}
				token := settleInterruptAwait(t, exit)
				before := logRecords(t, st, "t")
				err = a.SettleThread(t.Context(), token)
				if tc.fail {
					var interrupted *RunInterruptedError
					if !errors.Is(err, cause) || errors.As(err, &interrupted) {
						t.Fatalf("settle append error = %v, want unwrapped execution-free store error", err)
					}
					if len(fault.attempts) != 1 {
						t.Fatalf("first Settle silently retried %d times", len(fault.attempts))
					}
				} else if err != nil {
					t.Fatal(err)
				}
				if tc.fail && !tc.afterWrite {
					if !reflect.DeepEqual(logRecords(t, st, "t"), before) {
						t.Fatal("prewrite failure changed incomplete declaration")
					}
					if _, err := a.RunThread(t.Context(), "t", "new"); !errors.Is(err, ErrRunIncomplete) {
						t.Fatalf("failed settle unlocked new input: %v", err)
					}
				}
				// The same token confirms an acknowledged or ambiguous cancellation,
				// or retries a definitely unwritten cancellation.
				snapshot := settleInterruptSnapshot(t, a, token)
				settleInterruptUnknown(t, snapshot.History, "c1", "c2")
				after := logRecords(t, st, "t")
				equalKinds(t, logKinds(t, st, "t"), store.KindRunStarted, store.KindRoundDeclared, "run_cancelled")
				if !reflect.DeepEqual(after[:len(before)], before) {
					t.Fatal("settlement changed the original declaration")
				}
				if tc.fail {
					if len(fault.attempts) != 2 {
						t.Fatalf("append attempts = %d, want 2", len(fault.attempts))
					}
					first, second := fault.attempts[0], fault.attempts[1]
					if first.ID != second.ID || first.Schema != second.Schema || !bytes.Equal(first.Payload, second.Payload) {
						t.Fatal("retry changed cancellation identity or bytes")
					}
					for _, revision := range fault.revisions {
						if revision != token.ExpectedHead {
							t.Fatalf("retry refreshed revision to %d, want %d", revision, token.ExpectedHead)
						}
					}
				}
				if worker.calls.Load() != 0 || p.index != 1 {
					t.Fatalf("settlement reexecuted: tools=%d provider=%d", worker.calls.Load(), p.index)
				}
			})
		})
	}
}
