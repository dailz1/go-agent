package agenttool_test

import (
	"errors"
	"reflect"
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
			parentRegistry.MustRegister(agenttool.New(child, agenttool.Config{Name: "child",
				RunOptions: []agent.RunOption{agent.WithThread("")}}))
			parentProvider := &roundContextSequenceProvider{steps: [][]llm.Chunk{settleCall("parent-call", "child")}}
			parent := agent.New(parentProvider, parentRegistry, agent.WithStore(parentStore),
				agent.WithRunExitFn(func(token agent.SettlementToken) { parentExits <- token }))
			seq, err := parent.RunStream(t.Context(), "work", agent.WithThread("parent"))
			if err != nil {
				t.Fatal(err)
			}
			sawResult := false
			var deliveredContent string
			var deliveredIsError bool
			for event, err := range seq {
				if err != nil {
					t.Fatal(err)
				}
				if ev, ok := event.(agent.ToolResultEvent); ok {
					sawResult = true
					deliveredContent = ev.Result.Content
					deliveredIsError = ev.Result.IsError()
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
			// The parent broke at its first delivered result: that result is
			// recorded and the settlement must preserve it verbatim.
			snapshot, err := parent.ResumeThread(t.Context(), parentToken.ThreadID)
			if err != nil || snapshot == nil || !snapshot.Cancelled {
				t.Fatalf("cancelled snapshot = %+v, %v", snapshot, err)
			}
			if len(snapshot.History) != 3 || !reflect.DeepEqual(snapshot.Message, llm.Message{}) {
				t.Fatalf("cancellation imported child history or fabricated an answer: %+v", snapshot)
			}
			var blocks []*llm.ToolResultBlock
			for _, message := range snapshot.History {
				for _, block := range message.Content {
					if rb, ok := block.(*llm.ToolResultBlock); ok {
						blocks = append(blocks, rb)
					}
				}
			}
			if len(blocks) != 1 || blocks[0].ToolUseID != "parent-call" {
				t.Fatalf("result = %#v, want the delivered parent-call result", blocks)
			}
			if blocks[0].Content != deliveredContent || blocks[0].IsError != deliveredIsError {
				t.Fatalf("delivered result not preserved: %#v, want %q isError=%v", blocks[0], deliveredContent, deliveredIsError)
			}
			settleUnchanged(t, childStore, childToken.ThreadID, before)
			if gate.calls.Load() != 0 || len(provider.requests) != 1 || len(parentProvider.requests) != 1 {
				t.Fatal("terminal cleanup executed child work")
			}
		})
	}
}
