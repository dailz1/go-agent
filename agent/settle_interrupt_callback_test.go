package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

func TestSettleInterruptCallbackFailureE12(t *testing.T) {
	for _, name := range []string{"error", "cancel"} {
		t.Run(name, func(t *testing.T) {
			settleInterruptStores(t, func(t *testing.T, st store.Store) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				cause := errors.New("round context unavailable")
				if name == "cancel" {
					cause = context.Canceled
				}
				var overlays atomic.Int32
				worker := &parallelTestTool{info: tool.ToolInfo{Name: "one"},
					execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
						return tool.NewTextResult("saved result"), nil
					},
				}
				reg := tool.NewRegistry()
				reg.MustRegister(worker)
				p := &settleInterruptProvider{MockStreamingProvider: NewMockStreamingProvider(nil)}
				p.run = func(_ context.Context, _ []llm.Message, yield func(llm.Chunk, error) bool) {
					for _, chunk := range parallelToolRound(parallelToolCalls("one")) {
						if !yield(chunk, nil) {
							return
						}
					}
				}
				exit := make(chan SettlementToken, 1)
				a := New(p, reg, WithStore(st), WithLogger(discardLogger()),
					WithRunExitFn(func(token SettlementToken) { exit <- token }),
					WithRoundContextProvider(func(context.Context, RoundContextRequest) (string, error) {
						if overlays.Add(1) == 1 {
							return "ephemeral-overlay-A", nil
						}
						if name == "cancel" {
							cancel()
						}
						return "ephemeral-overlay-B", cause
					}))
				_, err := a.RunThread(ctx, "t", "old")
				token := settleInterruptToken(t, err, cause)
				if got := settleInterruptAwait(t, exit); got != token {
					t.Fatalf("callback error and exit tokens differ: %+v / %+v", token, got)
				}
				if p.calls.Load() != 1 || overlays.Load() != 2 {
					t.Fatalf("provider calls=%d overlay calls=%d", p.calls.Load(), overlays.Load())
				}
				request, err := json.Marshal(p.requests[0])
				if err != nil || !strings.Contains(string(request), "ephemeral-overlay-A") {
					t.Fatalf("first successful overlay never reached provider: %s, %v", request, err)
				}
				snapshot := settleInterruptSnapshot(t, a, token)
				history, err := json.Marshal(snapshot.History)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(history), "ephemeral-overlay-") {
					t.Fatalf("snapshot persisted overlay: %s", history)
				}
				for _, record := range logRecords(t, st, "t") {
					if strings.Contains(string(record.Payload), "ephemeral-overlay-") {
						t.Fatalf("durable overlay in %s: %s", record.Kind, record.Payload)
					}
				}
				if p.calls.Load() != 1 || overlays.Load() != 2 || worker.calls.Load() != 1 {
					t.Fatal("settlement executed callback, provider, or tool again")
				}
				got := historyResultBlocks(snapshot.History)
				if len(got) != 1 || got[0].Content != "saved result" || got[0].IsError {
					t.Fatalf("committed result lost after callback failure: %+v", got)
				}
			})
		})
	}
}
