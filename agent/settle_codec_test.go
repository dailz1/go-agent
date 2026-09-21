package agent

import (
	"reflect"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

func settleCodecPrefix(t *testing.T) []store.Record {
	t.Helper()
	declared := toolCallMessage("c1", "c2")
	declared.Content = append([]llm.ContentBlock{llm.ReasoningItemBlock{
		Type: "reasoning_item", ID: "reasoning", Summary: []string{"retained"},
		EncryptedContent: "opaque",
	}}, declared.Content...)
	return []store.Record{
		mustRecord(t, store.KindRunStarted, "r-start", runStartedPayload{
			RunID: "r", Input: "original", SystemPrompt: "frozen",
		}),
		mustRecord(t, store.KindRoundDeclared, "r-d0", roundDeclaredPayload{
			RunID: "r", Round: 0, Message: declared,
		}),
	}
}

func settleCodecCancel(t *testing.T) store.Record {
	t.Helper()
	prefix := settleCodecPrefix(t)
	var declaration roundDeclaredPayload
	if err := strictDecode(prefix[1].Payload, &declaration); err != nil {
		t.Fatal(err)
	}
	v := threadView{open: &openRound{declared: declaration.Message}}
	results := v.closeUnknown()
	return store.Record{Kind: "run_cancelled", Schema: 2, ID: "r-cancel", Payload: mustJSON(t, map[string]any{
		"run_id": "r", "open_round": 0, "results": results,
	})}
}

func settleCodecStore(records []store.Record) *scriptStore {
	for i := range records {
		records[i].Seq = int64(i)
	}
	return &scriptStore{records: records}
}

func TestSettleCodecClosesDeclaration(t *testing.T) {
	records := append(settleCodecPrefix(t), settleCodecCancel(t))
	a := New(nil, nil, WithStore(settleCodecStore(records)))
	v, err := a.replayThread(t.Context(), "t")
	if err != nil {
		t.Fatal(err)
	}
	if v.runActive || v.open != nil || len(v.history) != 4 {
		t.Fatalf("cancel replay = %+v", v)
	}
	if _, err := partitionHistory(seededHistory(v.system, v.history)); err != nil {
		t.Fatal(err)
	}
	reasoning := v.history[1].Content[0].(llm.ReasoningItemBlock)
	if reasoning.EncryptedContent != "opaque" || !reflect.DeepEqual(reasoning.Summary, []string{"retained"}) {
		t.Fatalf("reasoning lost: %+v", reasoning)
	}
	for i, id := range []string{"c1", "c2"} {
		res := toolResultBlocks(v.history[i+2])
		if len(res) != 1 || res[0].ToolUseID != id || !res[0].IsError {
			t.Fatalf("unknown result %d = %+v", i, res)
		}
	}
}

func TestSettleCodecPreservesCommittedResults(t *testing.T) {
	records := settleCodecPrefix(t)
	results := []llm.Message{
		llm.ToolResultMessage("c1", tool.NewTextResult("saved")),
		llm.ToolResultMessage("c2", tool.NewErrorResult("rejected")),
	}
	records = append(records, mustRecord(t, store.KindRoundCommitted, "r-c0",
		roundCommittedPayload{RunID: "r", Round: 0, Results: results}))
	cancel := settleCodecCancel(t)
	cancel.Payload = []byte(`{"run_id":"r","open_round":null,"results":[]}`)
	records = append(records, cancel)
	a := New(nil, nil, WithStore(settleCodecStore(records)))
	v, err := a.replayThread(t.Context(), "t")
	if err != nil {
		t.Fatal(err)
	}
	if v.runActive || v.open != nil || !reflect.DeepEqual(v.history[2:], results) {
		t.Fatalf("committed results changed: %+v", v)
	}
}
