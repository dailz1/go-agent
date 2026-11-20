package agent

import (
	"errors"
	"testing"

	"github.com/dailz1/go-agent/store"
)

func recoverySupersededFixture(t *testing.T, checkpoint bool) []store.Record {
	t.Helper()
	modelA := "A"
	config := mustRecord(t, store.KindConfigChanged, "cfg-a", configChangedPayload{Model: &modelA})
	config.Schema = store.SchemaV2
	log := append([]store.Record{config}, recoveryLogFixture(t)...)
	for _, i := range []int{2, 3, 5} {
		p, err := decodeContextRecovery(log[i])
		if err != nil {
			t.Fatal(err)
		}
		p.Model, p.ConfigSeq = modelA, 0
		log[i] = recoveryRecord(t, store.KindContextRecovery, p)
	}
	log = sequenceRecoveryLog(log)
	if checkpoint {
		v, err := replayRecords(threadView{lastRound: -1, cfgSeq: -1}, log)
		if err != nil {
			t.Fatal(err)
		}
		cp := recoveryCheckpointRecord(t, v)
		cp.ID = "cp"
		log = append(log, cp)
	}
	modelB, thinking, modelC := "B", "on", "C"
	modelChangeSeq := int64(len(log))
	for _, tc := range []struct {
		id string
		p  configChangedPayload
	}{
		{"cfg-b", configChangedPayload{Model: &modelB}},
		{"cfg-thinking", configChangedPayload{Thinking: &thinking}},
		{"cfg-c", configChangedPayload{Model: &modelC}},
	} {
		r := mustRecord(t, store.KindConfigChanged, tc.id, tc.p)
		r.Schema = store.SchemaV2
		log = append(log, r)
	}
	selectedSeq, provider := modelChangeSeq+1, "a"
	p, err := decodeContextRecovery(log[5])
	if err != nil {
		t.Fatal(err)
	}
	p.Phase, p.Step = "superseded", 3
	p.SelectedProvider, p.SelectedModel = &provider, &modelB
	p.SelectedConfigSeq, p.ModelChangeSeq = &selectedSeq, &modelChangeSeq
	log = append(log, recoveryRecord(t, store.KindContextRecovery, p))
	return sequenceRecoveryLog(log)
}

func TestRecoverySupersededUsesFrozenVersion(t *testing.T) {
	for _, checkpoint := range []bool{false, true} {
		t.Run(map[bool]string{false: "full log", true: "checkpoint tail"}[checkpoint], func(t *testing.T) {
			log := recoverySupersededFixture(t, checkpoint)
			a := New(nil, nil, WithStore(&scriptStore{records: log}))
			v, err := a.replayThread(t.Context(), "t")
			if err != nil || v.recovery != nil || v.cfg.Model == nil || *v.cfg.Model != "C" ||
				v.cfgSeq != int64(len(log)-2) {
				t.Fatalf("frozen B must supersede without rolling back latest C: %+v, %v", v, err)
			}
		})
	}
}

func TestRecoverySupersededRequiresSchemaTwoForEveryConfig(t *testing.T) {
	for _, endpoint := range []string{"original", "model change", "selected"} {
		for _, schema := range []int{store.SchemaV1, store.SchemaV3} {
			t.Run(endpoint+"/"+jsonNumber(schema), func(t *testing.T) {
				log := recoverySupersededFixture(t, false)
				r := log[len(log)-1]
				p, err := decodeContextRecovery(r)
				if err != nil {
					t.Fatal(err)
				}
				seq := p.ConfigSeq
				if endpoint == "model change" {
					seq = *p.ModelChangeSeq
				} else if endpoint == "selected" {
					seq = *p.SelectedConfigSeq
				}
				log[seq].Schema = schema
				if err := validateSuperseded(log, r, p); !errors.Is(err, ErrIncompatibleLog) {
					t.Fatalf("schema %d config at %s reset recovery: %v", schema, endpoint, err)
				}
			})
		}
	}
}

func TestRecoveryFailureNoteAllowsProvenSupersession(t *testing.T) {
	log := recoverySupersededFixture(t, false)
	p, err := decodeContextRecovery(log[5])
	if err != nil {
		t.Fatal(err)
	}
	p.Step, p.Reason = 3, "provider_error"
	note := recoveryRecord(t, store.KindContextRecovery, p)
	log = append(log[:6], append([]store.Record{note}, log[6:]...)...)
	superseded, err := decodeContextRecovery(log[len(log)-1])
	if err != nil {
		t.Fatal(err)
	}
	superseded.Step++
	*superseded.SelectedConfigSeq++
	*superseded.ModelChangeSeq++
	log[len(log)-1].Payload = mustJSON(t, superseded)
	v, err := replayRecords(threadView{lastRound: -1, cfgSeq: -1}, sequenceRecoveryLog(log))
	if err != nil || v.recovery != nil || v.cfg.Model == nil || *v.cfg.Model != "C" {
		t.Fatalf("proven supersession after failed dispatch: %+v, %v", v, err)
	}
}

func TestRecoverySupersededRejectsUnprovedSelection(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func([]store.Record, *contextRecoveryPayload) []store.Record
	}{
		{"latest pair substituted", func(log []store.Record, p *contextRecoveryPayload) []store.Record {
			*p.SelectedModel = "C"
			return log
		}},
		{"thinking version as change source", func(log []store.Record, p *contextRecoveryPayload) []store.Record {
			*p.ModelChangeSeq = *p.SelectedConfigSeq
			return log
		}},
		{"source beyond frozen version", func(log []store.Record, p *contextRecoveryPayload) []store.Record {
			*p.ModelChangeSeq = *p.SelectedConfigSeq + 1
			return log
		}},
		{"same value change", func(log []store.Record, p *contextRecoveryPayload) []store.Record {
			a := "A"
			log[*p.ModelChangeSeq].Payload = mustJSON(t, configChangedPayload{Model: &a})
			*p.SelectedModel = a
			return log
		}},
		{"thinking only", func(log []store.Record, p *contextRecoveryPayload) []store.Record {
			thinking := "on"
			log[*p.ModelChangeSeq].Payload = mustJSON(t, configChangedPayload{Thinking: &thinking})
			return log
		}},
		{"A B A cycle", func(log []store.Record, p *contextRecoveryPayload) []store.Record {
			a := "A"
			log[*p.SelectedConfigSeq].Payload = mustJSON(t, configChangedPayload{Model: &a})
			*p.ModelChangeSeq, *p.SelectedModel = *p.SelectedConfigSeq, a
			return log
		}},
		{"future selection", func(log []store.Record, p *contextRecoveryPayload) []store.Record {
			*p.SelectedConfigSeq = int64(len(log))
			return log
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := recoverySupersededFixture(t, true)
			p, err := decodeContextRecovery(log[len(log)-1])
			if err != nil {
				t.Fatal(err)
			}
			log = tc.mutate(log, &p)
			log[len(log)-1].Payload = mustJSON(t, p)
			a := New(nil, nil, WithStore(&scriptStore{records: log}))
			v, err := a.replayThread(t.Context(), "t")
			if !errors.Is(err, ErrIncompatibleLog) || v.recovery == nil {
				t.Fatalf("unproved supersession = %+v, %v", v.recovery, err)
			}
		})
	}
	t.Run("missing proof interval", func(t *testing.T) {
		log := recoverySupersededFixture(t, false)
		v, err := replayRecords(threadView{lastRound: -1, cfgSeq: -1}, log[:len(log)-1])
		if err != nil {
			t.Fatal(err)
		}
		v.proof = log[4:] // missing canonical records after the frozen o=0
		if _, err := replayRecords(v, log[len(log)-1:]); !errors.Is(err, ErrIncompatibleLog) {
			t.Fatalf("incomplete proof: %v", err)
		}
	})
}

func TestRecoverySettlementConfirmationSupersededPrefix(t *testing.T) {
	for _, tc := range []struct {
		name       string
		wrongID    bool
		wrongRun   bool
		wrongProof bool
	}{
		{"lost acknowledgement", false, false, false},
		{"wrong record identity", true, false, false},
		{"wrong payload identity", false, true, false},
		{"unproved superseded", false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := recoverySupersededFixture(t, true)
			v, err := replayRecords(threadView{lastRound: -1, cfgSeq: -1}, log)
			if err != nil {
				t.Fatal(err)
			}
			cancel, err := v.cancelRecord()
			if err != nil {
				t.Fatal(err)
			}
			h := int64(len(log))
			cancel.Seq = h
			if tc.wrongID {
				cancel.ID = "other-cancel"
			}
			if tc.wrongRun {
				cancel.Payload = []byte(`{"run_id":"other","open_round":null,"results":[]}`)
			}
			if tc.wrongProof {
				p, err := decodeContextRecovery(log[h-1])
				if err != nil {
					t.Fatal(err)
				}
				*p.ModelChangeSeq = *p.SelectedConfigSeq
				log[h-1].Payload = mustJSON(t, p)
			}
			st := store.NewMemory()
			if _, err := st.Append(t.Context(), "t", 0, append(log, cancel)...); err != nil {
				t.Fatal(err)
			}
			a := New(nil, nil, WithStore(st))
			target := SettlementToken{ThreadID: "t", RunID: "r", ExpectedHead: h}
			err = a.confirmSettlement(t.Context(), target)
			switch {
			case tc.wrongID:
				if !errors.Is(err, store.ErrRevisionConflict) {
					t.Fatalf("wrong identity: %v", err)
				}
			case tc.wrongRun || tc.wrongProof:
				if !errors.Is(err, ErrIncompatibleLog) {
					t.Fatalf("invalid cancellation/proof: %v", err)
				}
			default:
				if err != nil {
					t.Fatalf("idempotent confirmation: %v", err)
				}
				state, err := st.Latest(t.Context(), "t")
				if err != nil || state.Head != h+1 {
					t.Fatalf("confirmation appended another cancellation: %+v, %v", state, err)
				}
			}
		})
	}
}
