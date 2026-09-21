package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

func TestSettleInterruptParallelJoinE06(t *testing.T) {
	settleInterruptStores(t, func(t *testing.T, st store.Store) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		started := make(chan struct{}, 2)
		draining := make(chan struct{}, 2)
		release := make(chan struct{})
		unblock := sync.OnceFunc(func() { close(release) })
		defer unblock()
		reg := tool.NewRegistry()
		workers := make([]*parallelTestTool, 0, 2)
		for _, name := range []string{"one", "two"} {
			worker := &parallelTestTool{info: tool.ToolInfo{Name: name},
				execute: func(ctx context.Context, _ json.RawMessage) (*tool.ToolResult, error) {
					started <- struct{}{}
					<-ctx.Done()
					draining <- struct{}{}
					if name == "two" {
						<-release
					}
					return nil, ctx.Err()
				},
			}
			workers = append(workers, worker)
			reg.MustRegister(worker)
		}
		exits := make(chan SettlementToken, 1)
		a := New(parallelProvider(parallelToolCalls("one", "two")), reg,
			WithStore(st), WithToolConcurrency(2), WithLogger(discardLogger()),
			WithRunExitFn(func(token SettlementToken) { exits <- token }))
		outcome := make(chan error, 1)
		go func() {
			_, err := a.RunThread(ctx, "t", "old")
			outcome <- err
		}()
		for range 2 {
			settleInterruptAwait(t, started)
		}
		cancel()
		for range 2 {
			settleInterruptAwait(t, draining)
		}
		if err := a.SettleThread(t.Context(), SettlementToken{
			ThreadID: "t", RunID: "probe", ExpectedHead: 2,
		}); !errors.Is(err, ErrThreadBusy) {
			t.Fatalf("settle with last worker draining = %v", err)
		}
		select {
		case token := <-exits:
			t.Fatalf("exit before final worker joined: %+v", token)
		default:
		}
		unblock()
		token := settleInterruptToken(t, settleInterruptAwait(t, outcome), context.Canceled)
		if got := settleInterruptAwait(t, exits); got != token {
			t.Fatalf("exit token %+v differs from %+v", got, token)
		}
		snapshot := settleInterruptSnapshot(t, a, token)
		settleInterruptUnknown(t, snapshot.History, "c1", "c2")
		for _, worker := range workers {
			if worker.calls.Load() != 1 {
				t.Fatalf("%s executions = %d, want 1", worker.info.Name, worker.calls.Load())
			}
		}
		equalKinds(t, logKinds(t, st, "t"), store.KindRunStarted, store.KindRoundDeclared, "run_cancelled")
	})
}
