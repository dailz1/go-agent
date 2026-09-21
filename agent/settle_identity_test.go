package agent

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
)

func TestSettleLateCredentials(t *testing.T) {
	for _, settled := range []bool{false, true} {
		t.Run(map[bool]string{false: "E47_resumed_A", true: "E48_settled_A"}[settled], func(t *testing.T) {
			ctx := context.Background()
			st := store.NewMemory()
			frozen := make(chan SettlementToken, 1)
			exitGate := make(chan struct{})
			release := sync.OnceFunc(func() { close(exitGate) })
			defer release()
			ag := persistTestAgent(t, st, ErrResponse(context.Canceled))
			WithRunExitFn(func(target SettlementToken) { frozen <- target; <-exitGate })(ag)
			done := make(chan error, 1)
			go func() { _, err := ag.RunThread(ctx, "t", "A"); done <- err }()
			target := settleAwait(t, frozen)
			other := persistTestAgent(t, st, MsgResponse(llm.AssistantMessage("A done")), ErrResponse(context.Canceled))
			if _, err := other.ResumeThread(ctx, "t"); !errors.Is(err, ErrThreadBusy) {
				t.Fatalf("exit callback released ownership early: %v", err)
			}
			release()
			err := settleAwait(t, done)
			var interrupted *RunInterruptedError
			if !errors.As(err, &interrupted) || *interrupted.Settlement != target {
				t.Fatalf("error and exit credentials differ: %v", err)
			}
			if settled {
				if err := other.SettleThread(ctx, target); err != nil {
					t.Fatal(err)
				}
				other.provider = NewMockProvider(ErrResponse(context.Canceled))
			} else if _, err := other.ResumeThread(ctx, "t"); err != nil {
				t.Fatal(err)
			}
			if _, err := other.RunThread(ctx, "t", "B"); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			before, err := other.replayThread(ctx, "t")
			if err != nil || before.runID == target.RunID || !before.runActive {
				t.Fatalf("successor = %#v, %v", before, err)
			}
			err = ag.SettleThread(ctx, target)
			if settled && err != nil || !settled && !errors.Is(err, store.ErrRevisionConflict) {
				t.Fatalf("late settlement = %v", err)
			}
			after, err := other.replayThread(ctx, "t")
			if err != nil || !reflect.DeepEqual(before, after) || *interrupted.Settlement != target {
				t.Fatalf("late request changed successor or token: %#v, %v", after, err)
			}
		})
	}
}

func TestSettleIdentityAndRevision(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{"E49_wrong_run", "E49_recreated", "E50_same_run_advanced", "E52_inspection_race", "E24_done"} {
		t.Run(mode, func(t *testing.T) {
			st := store.NewMemory()
			head := stagedRunStarted(t, st, "t", "A", "old")
			ag := persistTestAgent(t, st, MsgResponse(toolCallMessage("c1")), MsgResponse(llm.AssistantMessage("done")), ErrResponse(context.Canceled))
			target := SettlementToken{ThreadID: "t", RunID: "A", ExpectedHead: head}
			switch mode {
			case "E49_wrong_run":
				target.RunID = "wrong"
			case "E49_recreated":
				if err := st.Delete(ctx, "t"); err != nil {
					t.Fatal(err)
				}
				stagedRunStarted(t, st, "t", "B", "new")
			case "E50_same_run_advanced":
				ag.provider = NewMockProvider(MsgResponse(toolCallMessage("c1")), ErrResponse(context.Canceled))
				if _, err := ag.ResumeThread(ctx, "t"); !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			case "E52_inspection_race", "E24_done":
				captured, err := ag.SettlementTarget(ctx, "t")
				if err != nil || *captured != target {
					t.Fatalf("inspection = %#v, %v", captured, err)
				}
				if _, err := ag.ResumeThread(ctx, "t"); err != nil {
					t.Fatal(err)
				}
				if mode == "E52_inspection_race" {
					if _, err := ag.RunThread(ctx, "t", "B"); !errors.Is(err, context.Canceled) {
						t.Fatal(err)
					}
				}
			}
			before := logRecords(t, st, "t")
			if err := ag.SettleThread(ctx, target); !errors.Is(err, store.ErrRevisionConflict) {
				t.Fatalf("stale token accepted: %v", err)
			}
			if !reflect.DeepEqual(before, logRecords(t, st, "t")) {
				t.Fatal("stale token changed log")
			}
			if mode == "E50_same_run_advanced" {
				fresh, err := ag.SettlementTarget(ctx, "t")
				if err != nil || fresh.RunID != target.RunID || fresh.ExpectedHead <= target.ExpectedHead {
					t.Fatalf("fresh explicit inspection = %#v, %v", fresh, err)
				}
				if err := ag.SettleThread(ctx, *fresh); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
