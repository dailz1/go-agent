package agenttool_test

import (
	"context"
	"errors"
	"testing"

	"github.com/dailz1/go-agent/agent"
)

func TestSettleE53ParentChildCancelOrder(t *testing.T) {
	for _, name := range []string{"memory independent", "memory shared", "jsonl independent", "jsonl shared"} {
		t.Run(name, func(t *testing.T) {
			parentStore, childStore := settleStores(t, name)
			f := newSettleFixture(t, parentStore, childStore)
			childID := await(t, f.childIDs)
			parentOriginal := settleObservedToken(t, parentStore, "parent")
			childOriginal := settleObservedToken(t, childStore, childID)
			if childID == "parent" || childOriginal.RunID == parentOriginal.RunID {
				t.Fatal("parent and child identities were conflated")
			}
			f.cancel()
			await(t, f.gate.cancelled)
			for _, target := range []struct {
				a     *agent.Agent
				token agent.SettlementToken
			}{{f.parent, parentOriginal}, {f.child, childOriginal}} {
				if err := target.a.SettleThread(t.Context(), target.token); !errors.Is(err, agent.ErrThreadBusy) {
					t.Fatalf("settle before child join = %v, want busy", err)
				}
			}
			select {
			case <-f.joined:
				t.Fatal("parent returned before child resource cleanup")
			default:
			}
			parent, child, err := f.interrupt(t)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation cause lost: %v", err)
			}
			if parent != parentOriginal || child != childOriginal {
				t.Fatalf("exit credentials changed: parent=%+v child=%+v", parent, child)
			}
			settleNestedTokens(t, err, parent, child)
			if err := f.child.SettleThread(t.Context(), child); err != nil {
				t.Fatal(err)
			}
			settleCancelled(t, f.child, child, "leaf-call")
			if err := f.parent.SettleThread(t.Context(), parent); err != nil {
				t.Fatal(err)
			}
			settleCancelled(t, f.parent, parent, "parent-call")
			settleOneCancellation(t, childStore, child)
			settleOneCancellation(t, parentStore, parent)
			if len(f.childProvider.requests) != 1 || len(f.parentProvider.requests) != 1 || f.gate.calls.Load() != 1 {
				t.Fatal("settlement or snapshot executed provider/tool work")
			}
			if len(f.parentExits) != 0 || len(f.childExits) != 0 {
				t.Fatal("settlement or snapshot emitted an execution callback")
			}
		})
	}
}
