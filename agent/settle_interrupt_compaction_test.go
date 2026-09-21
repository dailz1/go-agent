package agent

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

func TestSettleInterruptCompaction(t *testing.T) {
	for _, name := range []string{"E08_in_progress", "E09_checkpoint_committed", "E10_checkpoint_ambiguous"} {
		t.Run(name, func(t *testing.T) {
			settleInterruptStores(t, func(t *testing.T, backing store.Store) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				wantErr := errors.New("checkpoint acknowledgement lost")
				var st store.Store = backing
				if name == "E10_checkpoint_ambiguous" {
					st = &settleInterruptAppendStore{
						Store: backing, kind: store.KindCheckpoint, afterWrite: true, err: wantErr,
					}
				}
				entered := make(chan struct{})
				var compactions, overlays atomic.Int32
				compactor := CompactFunc(func(ctx context.Context, history []llm.Message, budget CompactionBudget) (CompactionResult, error) {
					compactions.Add(1)
					if name == "E08_in_progress" {
						close(entered)
						<-ctx.Done()
						return CompactionResult{}, ctx.Err()
					}
					return compactKeepSystem{}.Compact(ctx, history, budget)
				})
				p := NewMockStreamingProvider(nil).WithStreamError(context.Canceled)
				exit := make(chan SettlementToken, 1)
				a := New(p, tool.NewRegistry(), WithStore(st), WithSystemPrompt("frozen"),
					WithContextWindowTokens(300), WithCompactor(compactor), WithLogger(discardLogger()),
					WithRunExitFn(func(token SettlementToken) { exit <- token }),
					WithRoundContextProvider(func(context.Context, RoundContextRequest) (string, error) {
						overlays.Add(1)
						return "transient overlay", nil
					}))
				input := strings.Repeat("x", 1000)
				outcome := make(chan error, 1)
				events := make(chan CompactionEvent, 1)
				go func() {
					seq, err := a.RunThreadStream(ctx, "t", input)
					if err == nil {
						for event, streamErr := range seq {
							if streamErr != nil {
								err = streamErr
							}
							if compacted, ok := event.(CompactionEvent); ok {
								events <- compacted
								break
							}
						}
					}
					outcome <- err
				}()
				if name == "E08_in_progress" {
					settleInterruptAwait(t, entered)
					cancel()
				}
				err := settleInterruptAwait(t, outcome)
				token := settleInterruptAwait(t, exit)
				switch name {
				case "E08_in_progress":
					// Failed compaction may report either cancellation or the still-exceeded budget.
					if !errors.Is(err, context.Canceled) && !errors.Is(err, ErrCompactionBudgetExceeded) {
						t.Fatalf("compaction interruption = %v", err)
					}
					got := settleInterruptToken(t, err, err)
					if got != token {
						t.Fatal("compaction error token differs from exit token")
					}
				case "E09_checkpoint_committed":
					if err != nil {
						t.Fatal(err)
					}
					event := settleInterruptAwait(t, events)
					if !reflect.DeepEqual(event.History, []llm.Message{llm.SystemMessage("frozen")}) {
						t.Fatalf("compaction event history = %+v", event.History)
					}
				case "E10_checkpoint_ambiguous":
					got := settleInterruptToken(t, err, wantErr)
					if got != token || token.ExpectedHead != 1 {
						t.Fatalf("ambiguous append refreshed original token: %+v / %+v", token, got)
					}
					before, err := st.History(t.Context(), "t", 0)
					if err != nil {
						t.Fatal(err)
					}
					if err := a.SettleThread(t.Context(), token); !errors.Is(err, store.ErrRevisionConflict) {
						t.Fatalf("old token after uncertain checkpoint = %v", err)
					}
					after, err := st.History(t.Context(), "t", 0)
					if err != nil || !reflect.DeepEqual(before, after) {
						t.Fatalf("conflicting settle changed log: %v", err)
					}
					target, err := a.SettlementTarget(t.Context(), "t")
					if err != nil || target == nil {
						t.Fatalf("new explicit inspection = %+v, %v", target, err)
					}
					if target.RunID != token.RunID || target.ExpectedHead != 2 {
						t.Fatalf("new inspection target = %+v, original = %+v", target, token)
					}
					token = *target
				}
				state, err := st.Latest(t.Context(), "t")
				if err != nil {
					t.Fatal(err)
				}
				if (state.Checkpoint != nil) != (name != "E08_in_progress") {
					t.Fatalf("unexpected checkpoint: %+v", state.Checkpoint)
				}
				beforeOverlays := overlays.Load()
				snapshot := settleInterruptSnapshot(t, a, token)
				want := []llm.Message{llm.SystemMessage("frozen")}
				if name == "E08_in_progress" {
					want = append(want, llm.UserMessage(input))
				}
				if !reflect.DeepEqual(snapshot.History, want) {
					t.Fatalf("settled compaction history = %+v, want %+v", snapshot.History, want)
				}
				if compactions.Load() != 1 || overlays.Load() != beforeOverlays || p.index != 0 {
					t.Fatal("settlement or snapshot executed compactor, overlay, or provider")
				}
			})
		})
	}
}
