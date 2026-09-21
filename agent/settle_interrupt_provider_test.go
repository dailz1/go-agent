package agent

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

func TestSettleInterruptProvider(t *testing.T) {
	for _, name := range []string{"E01_before_reply", "E02_partial_stream", "E13_double_cancel"} {
		t.Run(name, func(t *testing.T) {
			settleInterruptStores(t, func(t *testing.T, st store.Store) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				entered := make(chan struct{})
				exited := make(chan SettlementToken, 1)
				outcome := make(chan error, 1)
				p := &settleInterruptProvider{MockStreamingProvider: NewMockStreamingProvider(nil)}
				p.run = func(ctx context.Context, _ []llm.Message, yield func(llm.Chunk, error) bool) {
					if p.calls.Load() > 1 {
						if !yield(llm.TextDeltaChunk{Text: "new answer"}, nil) {
							return
						}
						yield(llm.DoneChunk{FinishReason: "stop"}, nil)
						return
					}
					if name == "E02_partial_stream" {
						for _, chunk := range []llm.Chunk{
							llm.TextDeltaChunk{Text: "unfinished text"},
							llm.ReasoningDeltaChunk{Text: "unfinished reasoning"},
							llm.ToolCallStartChunk{Index: 0, ID: "partial-call", Name: "echo"},
							llm.ToolCallArgsChunk{Index: 0, Delta: `{"partial":`},
						} {
							if !yield(chunk, nil) {
								return
							}
						}
					}
					close(entered)
					<-ctx.Done()
					yield(nil, ctx.Err())
				}
				a := New(p, tool.NewRegistry(), WithStore(st), WithLogger(discardLogger()),
					WithRunExitFn(func(token SettlementToken) { exited <- token }))
				go func() {
					seq, err := a.RunThreadStream(ctx, "t", "old input")
					if err == nil {
						for event, streamErr := range seq {
							if _, ok := event.(DoneEvent); ok {
								err = errors.New("interrupted provider emitted Done")
							}
							if streamErr != nil {
								err = streamErr
							}
						}
					}
					outcome <- err
				}()
				settleInterruptAwait(t, entered)
				cancel()
				if name == "E13_double_cancel" {
					cancel()
				}
				token := settleInterruptToken(t, settleInterruptAwait(t, outcome), context.Canceled)
				if got := settleInterruptAwait(t, exited); got != token {
					t.Fatalf("exit token %+v differs from error token %+v", got, token)
				}
				equalKinds(t, logKinds(t, st, "t"), store.KindRunStarted)
				snapshot := settleInterruptSnapshot(t, a, token)
				if p.calls.Load() != 1 {
					t.Fatal("settlement or snapshot called the provider")
				}
				if !reflect.DeepEqual(snapshot.History, []llm.Message{llm.UserMessage("old input")}) {
					t.Fatalf("partial content became durable: %+v", snapshot.History)
				}
				equalKinds(t, logKinds(t, st, "t"), store.KindRunStarted, "run_cancelled")
				seq, err := a.RunThreadStream(t.Context(), "t", "new input")
				if err != nil {
					t.Fatal(err)
				}
				var done *DoneEvent
				for event, streamErr := range seq {
					if streamErr != nil {
						t.Fatal(streamErr)
					}
					if terminal, ok := event.(DoneEvent); ok {
						done = &terminal
					}
				}
				want := []llm.Message{llm.UserMessage("old input"), llm.UserMessage("new input")}
				if p.calls.Load() != 2 || !reflect.DeepEqual(p.requests[1], want) || done == nil {
					t.Fatalf("redirect requests = %+v, Done = %+v", p.requests, done)
				}
				if _, err := partitionHistory(done.History); err != nil {
					t.Fatal(err)
				}
				replayed, err := a.replayThread(t.Context(), "t")
				if err != nil || !reflect.DeepEqual(done.History, seededHistory(replayed.system, replayed.history)) {
					t.Fatalf("Done history differs from replay: %v", err)
				}
			})
		})
	}
}
