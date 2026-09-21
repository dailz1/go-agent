package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
)

func TestSettleE32PreservesEarlierCommittedRound(t *testing.T) {
	settleInterruptStores(t, func(t *testing.T, st store.Store) {
		ag := persistTestAgent(t, st, MsgResponse(toolCallMessage("first")), MsgResponse(toolCallMessage("second", "third")))
		var target SettlementToken
		WithRunExitFn(func(value SettlementToken) { target = value })(ag)
		seq, err := ag.RunThreadStream(t.Context(), "t", "old")
		if err != nil {
			t.Fatal(err)
		}
		for event, err := range seq {
			if err != nil {
				t.Fatal(err)
			}
			if result, ok := event.(ToolResultEvent); ok && result.ID == "second" {
				break
			}
		}
		if err := ag.SettleThread(context.Background(), target); err != nil {
			t.Fatal(err)
		}
		res, err := ag.ResumeThread(t.Context(), "t")
		if err != nil || !res.Cancelled {
			t.Fatalf("snapshot = %#v, %v", res, err)
		}
		if _, err := partitionHistory(res.History); err != nil {
			t.Fatal(err)
		}
		results := historyResultBlocks(res.History)
		if len(results) != 3 || results[0].ToolUseID != "first" || results[0].IsError {
			t.Fatalf("earlier committed result changed: %#v", results)
		}
		for i, id := range []string{"second", "third"} {
			if results[i+1].ToolUseID != id || !results[i+1].IsError || !strings.Contains(results[i+1].Content, "unknown") {
				t.Fatalf("open round result = %#v", results[i+1])
			}
		}
		if ag.provider.(*MockProvider).index != 2 {
			t.Fatal("settlement executed the model")
		}
		if res.History[1].Role != llm.RoleAssistant {
			t.Fatal("committed declaration lost")
		}
	})
}
