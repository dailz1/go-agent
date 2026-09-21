package agent

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
)

func settlementStore(t *testing.T, disk bool) (store.Store, func() store.Store) {
	t.Helper()
	if !disk {
		st := store.NewMemory()
		return st, func() store.Store { return st }
	}
	dir := t.TempDir()
	st, err := store.NewJSONL(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	})
	return st, func() store.Store {
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
		st, err = store.NewJSONL(dir)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
}

func TestSettleLifecycle(t *testing.T) {
	for _, disk := range []bool{false, true} {
		for _, input := range []string{"", "  only tests\n"} {
			t.Run(map[bool]string{false: "memory", true: "jsonl"}[disk]+"/"+input, func(t *testing.T) {
				ctx := context.Background()
				st, reopen := settlementStore(t, disk)
				ag := persistTestAgent(t, st, ErrResponse(context.Canceled))
				WithSystemPrompt("frozen")(ag)
				_, err := ag.RunThread(ctx, "t", "old input")
				var interrupted *RunInterruptedError
				if !errors.As(err, &interrupted) || interrupted.Settlement == nil {
					t.Fatalf("interruption = %v", err)
				}
				target := *interrupted.Settlement
				// E20/E45: explicit settlement unlocks adjacent exact user inputs,
				// but the no-settlement protection stays intact.
				if _, err := ag.RunThread(ctx, "t", input); !errors.Is(err, ErrRunIncomplete) {
					t.Fatalf("unsettled new input = %v", err)
				}
				if err := ag.SettleThread(ctx, target); err != nil {
					t.Fatal(err)
				}
				st = reopen() // E18/E21: the gap contains no accepted new input.
				next := persistTestAgent(t, st, MsgResponse(llm.AssistantMessage("new answer")))
				WithSystemPrompt("must not replace frozen")(next)
				if err := next.SettleThread(ctx, target); err != nil {
					t.Fatal(err)
				}
				before := logRecords(t, st, "t")
				for range 2 { // E19: repeated read, no execution or writes.
					res, err := next.ResumeThread(ctx, "t")
					if err != nil || !res.Cancelled || res.ThreadID != "t" ||
						!reflect.DeepEqual(res.Message, llm.Message{}) || res.ToolCalls != 0 ||
						res.Truncated || res.Retries != nil || res.Usage != (llm.Usage{}) || res.TotalUsage != (llm.Usage{}) {
						t.Fatalf("cancelled snapshot = %#v, %v", res, err)
					}
					if len(res.History) != 2 || messageString(res.History[1]) != "old input" {
						t.Fatalf("gap history = %#v", res.History)
					}
				}
				if !reflect.DeepEqual(before, logRecords(t, st, "t")) || next.provider.(*MockProvider).index != 0 {
					t.Fatal("snapshot read executed or wrote")
				}
				res, err := next.RunThread(ctx, "t", input)
				if err != nil {
					t.Fatal(err)
				}
				msgs := next.provider.(*MockProvider).LastMessages
				if len(msgs) != 3 || messageString(msgs[0]) != "frozen" ||
					msgs[1].Role != llm.RoleUser || msgs[2].Role != llm.RoleUser || messageString(msgs[2]) != input {
					t.Fatalf("new request = %#v", msgs)
				}
				if _, err := partitionHistory(res.History); err != nil {
					t.Fatal(err)
				}
				view, err := next.replayThread(ctx, "t")
				if err != nil || !reflect.DeepEqual(res.History, seededHistory(view.system, view.history)) || view.runID == target.RunID {
					t.Fatalf("new run replay = %#v, %v", view, err)
				}
				// E23: the original cancellation retry cannot change a successor.
				before = logRecords(t, st, "t")
				if err := next.SettleThread(ctx, target); err != nil || !reflect.DeepEqual(before, logRecords(t, st, "t")) {
					t.Fatalf("late settlement = %v", err)
				}
				if _, err := next.ResumeThread(ctx, "t"); !errors.Is(err, ErrNothingToResume) {
					t.Fatalf("completed successor = %v", err)
				}
			})
		}
	}
}

func TestSettleNewStartRecovery(t *testing.T) {
	for _, settle := range []bool{false, true} {
		t.Run(map[bool]string{false: "E44_original", true: "E22_successor"}[settle], func(t *testing.T) {
			ctx := context.Background()
			st, reopen := settlementStore(t, true)
			ag := persistTestAgent(t, st, ErrResponse(context.Canceled))
			_, err := ag.RunThread(ctx, "t", "old")
			var interrupted *RunInterruptedError
			if !errors.As(err, &interrupted) {
				t.Fatal(err)
			}
			wantInput := "old"
			if settle {
				if err := ag.SettleThread(ctx, *interrupted.Settlement); err != nil {
					t.Fatal(err)
				}
				next := persistTestAgent(t, st, ErrResponse(context.Canceled))
				if _, err := next.RunThread(ctx, "t", "new"); !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
				wantInput = "new"
			}
			st = reopen()
			ag = persistTestAgent(t, st, MsgResponse(llm.AssistantMessage("recovered")))
			res, err := ag.ResumeThread(ctx, "t")
			if err != nil || res.Cancelled {
				t.Fatalf("Resume = %#v, %v", res, err)
			}
			msgs := ag.provider.(*MockProvider).LastMessages
			if messageString(msgs[len(msgs)-1]) != wantInput {
				t.Fatalf("resumed wrong input: %#v", msgs)
			}
		})
	}
}
