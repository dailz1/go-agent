package agenttool_test

import (
	"context"
	"errors"
	"testing"

	"github.com/dailz1/go-agent/agent"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

func TestSettleE60NonpersistentChildAndMissingIdentity(t *testing.T) {
	t.Run("nonpersistent child only needs join", func(t *testing.T) {
		parentStore := store.NewMemory()
		f := newSettleFixture(t, parentStore, nil)
		if id := await(t, f.childIDs); id != "" {
			t.Fatalf("nonpersistent child acquired thread %q", id)
		}
		parent, child, err := f.interrupt(t)
		if !errors.Is(err, context.Canceled) || child != (agent.SettlementToken{}) || len(f.childExits) != 0 {
			t.Fatalf("nonpersistent child generated credential: %+v, %v", child, err)
		}
		wrappers := 0
		for current := err; current != nil; current = errors.Unwrap(current) {
			if _, ok := current.(*agent.RunInterruptedError); ok {
				wrappers++
			}
		}
		if wrappers != 1 {
			t.Fatalf("persistent wrappers = %d, want parent only", wrappers)
		}
		h := settleHarness{parent: settleOwnedTarget{f.parent, parent}}
		if _, err := h.cleanup(t.Context()); err != nil {
			t.Fatal(err)
		}
		settleCancelled(t, f.parent, parent, "parent-call")
		if _, err := f.child.SettlementTarget(t.Context(), "unavailable"); !errors.Is(err, agent.ErrNoStore) {
			t.Fatalf("nonpersistent child target = %v", err)
		}
		if len(f.childProvider.requests) != 1 || len(f.parentProvider.requests) != 1 || f.gate.calls.Load() != 1 {
			t.Fatal("nonpersistent cleanup executed work")
		}
	})
	t.Run("restart without durable child identity remains unconfirmed", func(t *testing.T) {
		parentDir, childDir := t.TempDir(), t.TempDir()
		parentStore, childStore := settleJSONL(t, parentDir), settleJSONL(t, childDir)
		f := newSettleFixture(t, parentStore, childStore)
		oracleChildID := await(t, f.childIDs)
		// Only the test oracle retains this ID. The restarted controller has no
		// durable index, as when the process dies before identity delivery.
		f.interrupt(t)
		childBefore := settleRecords(t, childStore, oracleChildID)
		parentBefore := settleRecords(t, parentStore, "parent")
		if err := parentStore.Close(); err != nil {
			t.Fatal(err)
		}
		if err := childStore.Close(); err != nil {
			t.Fatal(err)
		}
		parentStore, childStore = settleJSONL(t, parentDir), settleJSONL(t, childDir)
		provider := &roundContextSequenceProvider{}
		parent := agent.New(provider, tool.NewRegistry(), agent.WithStore(parentStore))
		target, err := parent.SettlementTarget(t.Context(), "parent")
		if err != nil || target == nil {
			t.Fatalf("explicit parent recovery inspection: %+v, %v", target, err)
		}
		h := settleHarness{parent: settleOwnedTarget{parent, *target}, expectedChildren: 1}
		if _, err := h.redirect(t.Context(), "must not run"); !errors.Is(err, errSettleIdentityMissing) {
			t.Fatalf("missing child identity reported clean: %v", err)
		}
		settleUnchanged(t, parentStore, "parent", parentBefore)
		settleUnchanged(t, childStore, oracleChildID, childBefore)
		if err := parent.SettleThread(t.Context(), *target); err != nil {
			t.Fatal(err)
		}
		if _, err := h.cleanup(t.Context()); !errors.Is(err, errSettleIdentityMissing) {
			t.Fatalf("parent settlement hid missing child identity: %v", err)
		}
		settleUnchanged(t, childStore, oracleChildID, childBefore)
		oracle := agent.New(provider, tool.NewRegistry(), agent.WithStore(childStore))
		if target, err := oracle.SettlementTarget(t.Context(), oracleChildID); err != nil || target == nil {
			t.Fatalf("unindexed child should remain incomplete: %+v, %v", target, err)
		}
		if len(provider.requests) != 0 {
			t.Fatal("missing-identity recovery executed provider")
		}
	})
}
