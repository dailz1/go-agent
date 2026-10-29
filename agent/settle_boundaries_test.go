package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

func TestSettlePublicBoundaries(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name string
		st   store.Store
		id   string
		want error
	}{
		{"E28_no_store", nil, "t", ErrNoStore},
		{"E28_empty_id", store.NewMemory(), "", ErrInvalidThreadID},
		{"E28_long_id", store.NewMemory(), strings.Repeat("/", 250), ErrInvalidThreadID},
		{"noncomparable", valueStore{calls: map[string]int{}}, "t", ErrStoreNotComparable},
	} {
		t.Run(test.name, func(t *testing.T) {
			ag := persistTestAgent(t, test.st)
			if err := ag.SettleThread(ctx, SettlementToken{ThreadID: test.id, RunID: "r", ExpectedHead: 1}); !errors.Is(err, test.want) {
				t.Fatalf("Settle = %v", err)
			}
			if _, err := ag.SettlementTarget(ctx, test.id); !errors.Is(err, test.want) {
				t.Fatalf("Target = %v", err)
			}
		})
	}
	st := store.NewMemory()
	ag := persistTestAgent(t, st)
	for _, target := range []SettlementToken{
		{ThreadID: "t"}, {ThreadID: "t", RunID: "r", ExpectedHead: -1},
		{ThreadID: "t", RunID: "r", ExpectedHead: 0}, {ThreadID: "t", RunID: "r", ExpectedHead: 1},
	} {
		if err := ag.SettleThread(ctx, target); !errors.Is(err, store.ErrRevisionConflict) {
			t.Fatalf("invalid/absent target %#v = %v", target, err)
		}
	}
}

func TestSettleBeforeSession(t *testing.T) {
	for _, mode := range []string{"E29_cancelled", "E29_unranged", "E30_ambiguous_start"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			st := &ambiguousStore{Store: store.NewMemory(), armed: mode == "E30_ambiguous_start"}
			ag := persistTestAgent(t, st)
			callbacks := 0
			WithRunExitFn(func(SettlementToken) { callbacks++ })(ag)
			var err error
			if mode == "E29_unranged" {
				_, err = ag.RunStream(ctx, "old", WithThread("t"))
			} else {
				if mode == "E29_cancelled" {
					cancel()
				}
				_, err = ag.Run(ctx, "old", WithThread("t"))
			}
			var interrupted *RunInterruptedError
			if errors.As(err, &interrupted) || callbacks != 0 {
				t.Fatalf("setup delivered execution credential: %v / %d callbacks", err, callbacks)
			}
			target, targetErr := ag.SettlementTarget(context.Background(), "t")
			if mode == "E30_ambiguous_start" {
				if err == nil || targetErr != nil || target == nil || target.ExpectedHead != 1 {
					t.Fatalf("uncertain start/inspection = %v, %#v, %v", err, target, targetErr)
				}
				if err := ag.SettleThread(context.Background(), *target); err != nil {
					t.Fatal(err)
				}
			} else {
				if !errors.Is(targetErr, ErrNothingToSettle) {
					t.Fatal(targetErr)
				}
				if _, err := ag.ResumeThread(context.Background(), "t"); !errors.Is(err, ErrNothingToResume) {
					t.Fatal(err)
				}
				state, err := st.Latest(context.Background(), "t")
				if err != nil || state.Head != 0 {
					t.Fatalf("setup wrote: %#v, %v", state, err)
				}
			}
		})
	}
}

type settleCancelAfterAppend struct {
	store.Store
	cancel context.CancelFunc
}

func (s *settleCancelAfterAppend) Append(ctx context.Context, id string, h int64, records ...store.Record) (int64, error) {
	head, err := s.Store.Append(ctx, id, h, records...)
	if err == nil && len(records) > 0 && records[0].Kind == store.KindRunCancelled {
		s.cancel()
	}
	return head, err
}

func TestSettleCancellationContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	st := &settleCancelAfterAppend{Store: store.NewMemory(), cancel: cancel}
	ag := persistTestAgent(t, st)
	head := stagedRunStarted(t, st, "t", "r", "old")
	target := SettlementToken{ThreadID: "t", RunID: "r", ExpectedHead: head}
	cancel()
	if err := ag.SettleThread(ctx, target); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	equalKinds(t, logKinds(t, st, "t"), store.KindRunStarted)
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	st.cancel = cancel
	if err := ag.SettleThread(ctx, target); err != nil || !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("E39 confirmed append revoked: %v, ctx=%v", err, ctx.Err())
	}
}

// TestSettleAfterCooperativeEndDoesNotReopen pins that a run closed by
// run_closed is a real terminal: the original token reports nothing to
// settle, and after a later append the same stale token conflicts instead
// of cancelling or reopening anything.
func TestSettleAfterCooperativeEndDoesNotReopen(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	reg := tool.NewRegistry()
	reg.MustRegister(mutatorTool("calc", func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult("42"), nil
	}))
	provider := NewMockStreamingProvider([][]llm.Chunk{toolCallChunks("c1", "calc")})
	var token SettlementToken
	agent := New(provider, reg, WithStore(st), WithLogger(discardLogger()),
		WithFinishRound(func(context.Context, FinishRoundContext) (FinishRoundDecision, error) {
			return FinishRoundDecision{Action: FinishRoundEnd}, nil
		}),
		WithRunExitFn(func(tk SettlementToken) { token = tk }),
	)
	if _, err := agent.Run(ctx, "compute", WithThread("t")); err != nil {
		t.Fatalf("run: %v", err)
	}
	if token.ExpectedHead != 4 {
		t.Fatalf("exit token ExpectedHead = %d, want 4 (run_closed included)", token.ExpectedHead)
	}
	if err := agent.SettleThread(ctx, token); err != ErrNothingToSettle {
		t.Fatalf("Settle after cooperative end = %v, want ErrNothingToSettle", err)
	}
	if _, err := agent.EnqueueInputWithID(ctx, "t", "k-after", "after end"); err != nil {
		t.Fatal(err)
	}
	if err := agent.SettleThread(ctx, token); !errors.Is(err, store.ErrRevisionConflict) {
		t.Fatalf("Settle with the stale token after a later append = %v, want store.ErrRevisionConflict", err)
	}
	equalKinds(t, logKinds(t, st, "t"),
		store.KindRunStarted, store.KindRoundDeclared, store.KindRoundCommitted,
		store.KindRunClosed, store.KindInputQueued)
}

func TestSettleHistoryEntrypointsStayNonpersistent(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		t.Run(map[bool]string{false: "without_store", true: "with_store"}[persistent], func(t *testing.T) {
			st := store.NewMemory()
			ag := persistTestAgent(t, st, MsgResponse(llm.AssistantMessage("done")), MsgResponse(llm.AssistantMessage("done")))
			if !persistent {
				ag.store = nil
			}
			callbacks := 0
			WithRunExitFn(func(SettlementToken) { callbacks++ })(ag)
			res, err := ag.RunWithHistory(context.Background(), []llm.Message{llm.UserMessage("old")}, "new")
			if err != nil || res.ThreadID != "" {
				t.Fatalf("RunWithHistory = %#v, %v", res, err)
			}
			seq, err := ag.RunStreamWithHistory(context.Background(), []llm.Message{llm.UserMessage("old")}, "new")
			if err != nil {
				t.Fatal(err)
			}
			for event, err := range seq {
				if err != nil {
					t.Fatal(err)
				}
				if done, ok := event.(DoneEvent); ok && done.ThreadID != "" {
					t.Fatal(done.ThreadID)
				}
			}
			if callbacks != 0 {
				t.Fatal("E38 nonpersistent execution delivered token")
			}
			if persistent {
				if err := ag.SettleThread(context.Background(), SettlementToken{ThreadID: "t", RunID: "r", ExpectedHead: 1}); !errors.Is(err, store.ErrRevisionConflict) {
					t.Fatal(err)
				}
				state, err := st.Latest(context.Background(), "t")
				if err != nil || state.Head != 0 {
					t.Fatalf("imported nonpersistent history: %#v %v", state, err)
				}
			}
		})
	}
}
