package agent

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
)

func TestSettleCodecCheckpointVersions(t *testing.T) {
	for _, schema := range []int{store.SchemaV1, store.SchemaV2} {
		t.Run(string(rune('0'+schema)), func(t *testing.T) {
			start := settleCodecPrefix(t)[0]
			cp := store.Record{Kind: store.KindCheckpoint, Schema: schema, ID: "cp",
				Payload: []byte(`{"history":[{"role":"system","content":"frozen"},{"role":"user","content":"compacted"}],"system":"frozen","run_active":true,"run_id":"r","last_round":-1}`)}
			if schema == store.SchemaV2 {
				cp.Payload = append([]byte(`{"codec_version":2,`), cp.Payload[1:]...)
			}
			cancel := settleCodecCancel(t)
			cancel.Payload = []byte(`{"run_id":"r","open_round":null,"results":[]}`)
			st := settleCodecStore([]store.Record{start, cp, cancel})
			a := New(nil, nil, WithStore(st))
			v, err := a.replayThread(t.Context(), "t")
			if err != nil || !v.cancelled || len(v.history) != 2 || messageString(v.history[1]) != "compacted" {
				t.Fatalf("checkpoint -> cancellation = %+v, %v", v, err)
			}
		})
	}
}

func TestSettleCodecCheckpointAfterCancellation(t *testing.T) {
	st := store.NewMemory()
	records := append(settleCodecPrefix(t), settleCodecCancel(t))
	records = append(records, mustRecord(t, store.KindRunStarted, "next-start",
		runStartedPayload{RunID: "next", Input: "new input"}))
	if _, err := st.Append(t.Context(), "t", 0, records...); err != nil {
		t.Fatal(err)
	}
	a := New(nil, nil, WithStore(st))
	history := []llm.Message{llm.SystemMessage("frozen"), llm.UserMessage("compacted after cancel")}
	p := &persistence{a: a, thread: "t", head: 4, system: "frozen", runID: "next", lastRound: -1, open: -1}
	if err := p.checkpoint(t.Context(), history); err != nil {
		t.Fatal(err)
	}
	state, err := st.Latest(t.Context(), "t")
	if err != nil {
		t.Fatal(err)
	}
	var version struct {
		CodecVersion int `json:"codec_version"`
	}
	if err := json.Unmarshal(state.Checkpoint.Payload, &version); err != nil {
		t.Fatal(err)
	}
	if state.Checkpoint.Schema != store.SchemaV2 || version.CodecVersion != 2 {
		t.Fatalf("checkpoint versions = schema %d, codec %d", state.Checkpoint.Schema, version.CodecVersion)
	}
	v, err := a.replayThread(t.Context(), "t")
	if err != nil || v.cancelled || !v.runActive || v.runID != "next" || !reflect.DeepEqual(v.history, history) {
		t.Fatalf("checkpoint replay = %+v, %v", v, err)
	}
	// A historical token must still validate through History, not Latest.
	if err := a.SettleThread(t.Context(), SettlementToken{ThreadID: "t", RunID: "r", ExpectedHead: 2}); err != nil {
		t.Fatalf("old token behind checkpoint: %v", err)
	}
	after, err := st.Latest(t.Context(), "t")
	if err != nil || after.Head != state.Head {
		t.Fatalf("old token changed later run: %+v, %v", after, err)
	}
}

func TestSettleCodecRejectsFutureVersions(t *testing.T) {
	for _, tc := range []struct {
		name    string
		schema  int
		payload string
	}{
		{"envelope", 3, `{}`},
		{"future codec", 2, `{"codec_version":3}`},
		{"future codec v1 envelope", 1, `{"codec_version":3}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cp := store.Record{Kind: store.KindCheckpoint, Schema: tc.schema, ID: "cp", Payload: []byte(tc.payload)}
			records := []store.Record{settleCodecPrefix(t)[0], cp}
			a := New(nil, nil, WithStore(settleCodecStore(records)))
			if _, err := a.replayThread(t.Context(), "t"); !errors.Is(err, ErrIncompatibleLog) {
				t.Fatalf("future checkpoint = %v, want incompatible", err)
			}
			if _, err := replayRecords(threadView{lastRound: -1}, records); !errors.Is(err, ErrIncompatibleLog) {
				t.Fatalf("future checkpoint in full prefix = %v, want incompatible", err)
			}
		})
	}
}
