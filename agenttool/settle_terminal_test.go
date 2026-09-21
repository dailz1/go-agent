package agenttool_test

import (
	"errors"
	"testing"

	"github.com/dailz1/go-agent/agent"
	"github.com/dailz1/go-agent/agenttool"
	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

func TestSettleE56AlreadyTerminalChild(t *testing.T) {
	for _, name := range []string{"done", "max iteration done"} {
		t.Run(name, func(t *testing.T) {
			childStore, parentStore := store.NewMemory(), store.NewMemory()
			childExits, parentExits := make(chan agent.SettlementToken, 1), make(chan agent.SettlementToken, 1)
			reply := settleText()
			if name == "max iteration done" {
				reply = settleCall("skipped-call", "leaf")
			}
			gate := newSettleGate(t)
			registry := tool.NewRegistry()
			registry.MustRegister(gate)
			provider := &roundContextSequenceProvider{steps: [][]llm.Chunk{reply}}
			child := agent.New(provider, registry, agent.WithStore(childStore), agent.WithMaxIter(1),
				agent.WithRunExitFn(func(token agent.SettlementToken) { childExits <- token }))
			parentRegistry := tool.NewRegistry()
			parentRegistry.MustRegister(agenttool.New(child, agenttool.Config{Name: "child"}))
			parentProvider := &roundContextSequenceProvider{steps: [][]llm.Chunk{settleCall("parent-call", "child")}}
			parent := agent.New(parentProvider, parentRegistry, agent.WithStore(parentStore),
				agent.WithRunExitFn(func(token agent.SettlementToken) { parentExits <- token }))
			seq, err := parent.RunThreadStream(t.Context(), "parent", "work")
			if err != nil {
				t.Fatal(err)
			}
			sawResult := false
			for event, err := range seq {
				if err != nil {
					t.Fatal(err)
				}
				if _, ok := event.(agent.ToolResultEvent); ok {
					sawResult = true
					break
				}
			}
			if !sawResult {
				t.Fatal("did not reach parent result before its durable commit")
			}
			childToken, parentToken := await(t, childExits), await(t, parentExits)
			before := settleRecords(t, childStore, childToken.ThreadID)
			if err := child.SettleThread(t.Context(), childToken); !errors.Is(err, agent.ErrNothingToSettle) {
				t.Fatalf("terminal child settlement = %v", err)
			}
			if _, err := child.ResumeThread(t.Context(), childToken.ThreadID); !errors.Is(err, agent.ErrNothingToResume) {
				t.Fatalf("terminal child reopened: %v", err)
			}
			if err := parent.SettleThread(t.Context(), parentToken); err != nil {
				t.Fatal(err)
			}
			settleCancelled(t, parent, parentToken, "parent-call")
			settleUnchanged(t, childStore, childToken.ThreadID, before)
			if gate.calls.Load() != 0 || len(provider.requests) != 1 || len(parentProvider.requests) != 1 {
				t.Fatal("terminal cleanup executed child work")
			}
		})
	}
}
