package agent

import (
	"encoding/json"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
)

func TestSettleCodecCorruptCheckpointsFallBack(t *testing.T) {
	for _, schema := range []int{store.SchemaV1, store.SchemaV2} {
		for _, payload := range []string{
			`{"history":"broken"}`,
			`{"codec_version":2,"history":[],"run_active":false}`,
			`{"history":[],"system":"frozen","run_active":true,"run_id":"r","last_round":0}`,
		} {
			t.Run(payload, func(t *testing.T) {
				records := append(settleCodecPrefix(t), store.Record{
					Kind: store.KindCheckpoint, Schema: schema, ID: "bad-cp", Payload: []byte(payload),
				}, settleCodecCancel(t))
				a := New(nil, nil, WithStore(settleCodecStore(records)))
				v, err := a.replayThread(t.Context(), "t")
				if err != nil || !v.cancelled || v.open != nil || len(v.history) != 4 {
					t.Fatalf("corrupt checkpoint fallback = %+v, %v", v, err)
				}
				if _, err := partitionHistory(seededHistory(v.system, v.history)); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestSettleCodecCheckpointCannotMasqueradeAsV1(t *testing.T) {
	st := store.NewMemory()
	a := New(nil, nil, WithStore(st))
	p := &persistence{a: a, thread: "t", runID: "r", lastRound: -1, open: -1}
	if err := p.checkpoint(t.Context(), []llm.Message{llm.UserMessage("compacted")}); err != nil {
		t.Fatal(err)
	}
	state, err := st.Latest(t.Context(), "t")
	if err != nil {
		t.Fatal(err)
	}
	// This is the complete pre-R07 checkpoint DTO. Its strict decoder must
	// reject the new marker, forcing that reader back to the full log.
	var legacy struct {
		History   []llm.Message `json:"history"`
		System    string        `json:"system"`
		RunActive bool          `json:"run_active"`
		RunID     string        `json:"run_id,omitempty"`
		LastRound int           `json:"last_round"`
	}
	if err := strictDecode(state.Checkpoint.Payload, &legacy); err == nil {
		t.Fatal("new checkpoint was accepted as a v1 checkpoint")
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(state.Checkpoint.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if state.Checkpoint.Schema <= store.SchemaV1 || string(payload["codec_version"]) != "2" {
		t.Fatalf("legacy version guards cannot reject checkpoint: %+v", state.Checkpoint)
	}
}
