package agenttool_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/dailz1/go-agent/agent"
	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

func TestSettleE54ChildOnly(t *testing.T) {
	for _, name := range []string{"memory independent", "memory shared", "jsonl independent", "jsonl shared"} {
		t.Run(name, func(t *testing.T) {
			parentStore, childStore := settleStores(t, name)
			f := newSettleFixture(t, parentStore, childStore)
			parent, child, _ := f.interrupt(t)
			before := settleRecords(t, parentStore, parent.ThreadID)
			settler := f.child
			if parentStore == childStore {
				settler = f.parent
			} else if err := f.parent.SettleThread(t.Context(), child); !errors.Is(err, store.ErrRevisionConflict) {
				t.Fatalf("child credential routed to wrong store = %v", err)
			}
			if err := settler.SettleThread(t.Context(), child); err != nil {
				t.Fatal(err)
			}
			settleCancelled(t, f.child, child, "leaf-call")
			settleUnchanged(t, parentStore, parent.ThreadID, before)
			if _, err := f.parent.RunThread(t.Context(), parent.ThreadID, "redirect"); !errors.Is(err, agent.ErrRunIncomplete) {
				t.Fatalf("child-only cleanup unlocked parent: %v", err)
			}
			settleUnchanged(t, parentStore, parent.ThreadID, before)
			if len(f.parentProvider.requests) != 1 || len(f.childProvider.requests) != 1 || f.gate.calls.Load() != 1 {
				t.Fatal("child-only settlement executed work")
			}
		})
	}
}

func TestSettleE55ParentOnly(t *testing.T) {
	for _, name := range []string{"memory independent", "memory shared", "jsonl independent", "jsonl shared"} {
		t.Run(name, func(t *testing.T) {
			parentStore, childStore := settleStores(t, name)
			f := newSettleFixture(t, parentStore, childStore)
			parent, child, _ := f.interrupt(t)
			before := settleRecords(t, childStore, child.ThreadID)
			if err := f.parent.SettleThread(t.Context(), parent); err != nil {
				t.Fatal(err)
			}
			snapshot := settleCancelled(t, f.parent, parent, "parent-call")
			pending, err := f.child.SettlementTarget(t.Context(), child.ThreadID)
			if err != nil || pending == nil || *pending != child {
				t.Fatalf("child cleanup must remain pending: %+v, %v", pending, err)
			}
			settleUnchanged(t, childStore, child.ThreadID, before)

			f.parentProvider.steps = [][]llm.Chunk{settleCall("new-call", "child"), settleText()}
			input := "  only the test directory\n"
			seq, err := f.parent.RunThreadStream(t.Context(), parent.ThreadID, input)
			if err != nil {
				t.Fatal(err)
			}
			var done *agent.DoneEvent
			for event, err := range seq {
				if err != nil {
					t.Fatal(err)
				}
				if event, ok := event.(agent.DoneEvent); ok {
					done = &event
				}
			}
			newChild := await(t, f.childExits)
			if newChild.ThreadID == child.ThreadID || newChild.RunID == child.RunID {
				t.Fatalf("new delegation resumed old child: %+v", newChild)
			}
			request := f.parentProvider.requests[1]
			want := append(append([]llm.Message(nil), snapshot.History...), llm.UserMessage(input))
			if !reflect.DeepEqual(request, want) {
				t.Fatalf("redirect request lost canonical history or input: %+v", request)
			}
			if done == nil || len(done.History) != len(want)+3 {
				t.Fatalf("new parent run did not finish with paired history: %+v", done)
			}
			call := llm.ToolUseBlock{ID: "new-call", Name: "child", Input: []byte(`{"input":"child work"}`)}
			wantDone := append(want, llm.AssistantToolCallMessage(call),
				llm.ToolResultMessage("new-call", tool.NewTextResult("finished")), llm.AssistantMessage("finished"))
			if !reflect.DeepEqual(done.History, wantDone) {
				t.Fatalf("new delegation history is not paired: %+v", done.History)
			}
			replay := &roundContextSequenceProvider{steps: [][]llm.Chunk{settleText()}}
			reader := agent.New(replay, tool.NewRegistry(), agent.WithStore(parentStore))
			if _, err := reader.RunThread(t.Context(), parent.ThreadID, "replay probe"); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(replay.requests[0], append(wantDone, llm.UserMessage("replay probe"))) {
				t.Fatal("durable replay differs from final Done history")
			}
			if len(f.childProvider.requests) != 2 || f.gate.calls.Load() != 1 {
				t.Fatal("redirect replayed old child tool")
			}
			settleUnchanged(t, childStore, child.ThreadID, before)
			if _, err := f.child.RunThread(t.Context(), child.ThreadID, "not cleaned"); !errors.Is(err, agent.ErrRunIncomplete) {
				t.Fatalf("old child incorrectly cleaned: %v", err)
			}
		})
	}
}
