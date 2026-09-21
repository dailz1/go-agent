package agent

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
)

func TestSettleCodecFinitePrefixAndPureRecord(t *testing.T) {
	records := settleCodecStore(append(settleCodecPrefix(t), settleCodecCancel(t))).records
	v, err := replayRecords(threadView{lastRound: -1}, records[:2])
	if err != nil {
		t.Fatal(err)
	}
	before := mustJSON(t, v.open.declared)
	rec, err := v.cancelRecord()
	if err != nil {
		t.Fatal(err)
	}
	if !v.runActive || v.cancelled || v.open == nil || v.head != 2 || len(v.history) != 1 {
		t.Fatalf("record construction mutated prefix: %+v", v)
	}
	if !bytes.Equal(before, mustJSON(t, v.open.declared)) {
		t.Fatal("record construction mutated declaration")
	}
	again, err := v.cancelRecord()
	if err != nil || !reflect.DeepEqual(rec, again) {
		t.Fatalf("retry changed caller content: %+v, %v", again, err)
	}
	if rec.Schema != store.SchemaV2 || rec.Kind != store.KindRunCancelled || rec.ID != "r-cancel" {
		t.Fatalf("record identity = %+v", rec)
	}
	settled, err := replayRecords(v, records[2:])
	if err != nil || !settled.cancelled || settled.runActive || settled.head != 3 {
		t.Fatalf("finite cancellation = %+v, %v", settled, err)
	}
	// The prefix remains reusable for exact-target validation.
	if v.cancelled || v.open == nil || len(v.history) != 1 {
		t.Fatalf("finite replay mutated supplied view: %+v", v)
	}
	start := mustRecord(t, store.KindRunStarted, "next-start", runStartedPayload{RunID: "next", Input: ""})
	start.Seq = 3
	next, err := replayRecords(settled, []store.Record{start})
	if err != nil || next.cancelled || !next.runActive || next.lastRound != -1 || next.runID != "next" {
		t.Fatalf("next run = %+v, %v", next, err)
	}
	if next.history[len(next.history)-1].Role != llm.RoleUser {
		t.Fatal("new input missing")
	}
}
