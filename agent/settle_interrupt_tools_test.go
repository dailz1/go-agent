package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

func TestSettleInterruptTools(t *testing.T) {
	for _, tc := range []struct {
		name     string
		partial  bool
		approval bool
	}{
		{name: "E03_tool_cancel"},
		{name: "E04_partial_live_results", partial: true},
		{name: "E11_approval_wait", approval: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settleInterruptStores(t, func(t *testing.T, st store.Store) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				entered, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
				unblock := sync.OnceFunc(func() { close(release) })
				defer unblock()
				wait := func() {
					close(entered)
					<-ctx.Done()
					close(cancelled)
					<-release
				}
				var executions, approvals, liveResults atomic.Int32
				reg := tool.NewRegistry()
				reg.MustRegister(&parallelTestTool{
					info: tool.ToolInfo{Name: "slow", RequiresApproval: tc.approval},
					execute: func(ctx context.Context, _ json.RawMessage) (*tool.ToolResult, error) {
						executions.Add(1)
						wait()
						return nil, ctx.Err()
					},
				})
				names := []string{"slow"}
				if tc.partial {
					names = []string{"fast", "slow"}
					reg.MustRegister(&parallelTestTool{info: tool.ToolInfo{Name: "fast"},
						execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
							executions.Add(1)
							return tool.NewTextResult("live success"), nil
						},
					})
				}
				p := parallelProvider(parallelToolCalls(names...))
				exit := make(chan SettlementToken, 1)
				a := New(p, reg, WithStore(st), WithLogger(discardLogger()),
					WithRunExitFn(func(token SettlementToken) { exit <- token }),
					WithApprovalFn(func(tool.ToolInfo, json.RawMessage) bool {
						approvals.Add(1)
						wait()
						return false
					}))
				outcome := make(chan error, 1)
				go func() {
					seq, err := a.RunThreadStream(ctx, "t", "old")
					if err == nil {
						for event, streamErr := range seq {
							if _, ok := event.(ToolResultEvent); ok {
								liveResults.Add(1)
							}
							if streamErr != nil {
								err = streamErr
							}
						}
					}
					outcome <- err
				}()
				settleInterruptAwait(t, entered)
				if tc.partial && liveResults.Load() != 1 {
					t.Fatalf("live results = %d, want first success before second tool", liveResults.Load())
				}
				cancel()
				settleInterruptAwait(t, cancelled)
				probe := SettlementToken{ThreadID: "t", RunID: "probe", ExpectedHead: 1}
				if err := a.SettleThread(t.Context(), probe); !errors.Is(err, ErrThreadBusy) {
					t.Fatalf("settle during cleanup = %v, want busy", err)
				}
				select {
				case token := <-exit:
					t.Fatalf("exit callback ran before owned work joined: %+v", token)
				default:
				}
				unblock()
				token := settleInterruptToken(t, settleInterruptAwait(t, outcome), context.Canceled)
				if got := settleInterruptAwait(t, exit); got != token {
					t.Fatalf("exit token = %+v, want %+v", got, token)
				}
				equalKinds(t, logKinds(t, st, "t"), store.KindRunStarted, store.KindRoundDeclared)
				beforeExecutions, beforeApprovals := executions.Load(), approvals.Load()
				snapshot := settleInterruptSnapshot(t, a, token)
				ids := []string{"c1"}
				if tc.partial {
					ids = append(ids, "c2")
				}
				settleInterruptUnknown(t, snapshot.History, ids...)
				equalKinds(t, logKinds(t, st, "t"), store.KindRunStarted, store.KindRoundDeclared, "run_cancelled")
				if executions.Load() != beforeExecutions || approvals.Load() != beforeApprovals {
					t.Fatal("settlement executed or approved tools again")
				}
				if tc.approval && (beforeExecutions != 0 || beforeApprovals != 1) {
					t.Fatalf("approval calls=%d executions=%d", beforeApprovals, beforeExecutions)
				}
				if len(snapshot.History) != 2+len(ids) || snapshot.History[1].Role != llm.RoleAssistant {
					t.Fatalf("declaration was lost or terminal reply fabricated: %+v", snapshot.History)
				}
			})
		})
	}
}
