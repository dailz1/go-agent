package agent

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
)

func recoveryRecord(t *testing.T, kind string, payload any) store.Record {
	t.Helper()
	r := mustRecord(t, kind, "recovery", payload)
	r.Schema = store.SchemaV4
	return r
}

func recoveryPayloadFixture() contextRecoveryPayload {
	return contextRecoveryPayload{
		RunID: "r", Round: 0, EpisodeID: "episode", Step: 0,
		Provider: "a", Model: "", ConfigSeq: -1, Phase: "pending",
		BeforeRunes: 2000, RejectedOutboundRunes: 2100, Code: "context_length_exceeded",
	}
}

func mutateRecoveryJSON(t *testing.T, r store.Record, key string, value json.RawMessage) store.Record {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(r.Payload, &fields); err != nil {
		t.Fatal(err)
	}
	if value == nil {
		delete(fields, key)
	} else {
		fields[key] = value
	}
	r.Payload = mustJSON(t, fields)
	return r
}

func TestContextRecoveryCodec(t *testing.T) {
	pending := recoveryPayloadFixture()
	preparing := pending
	preparing.Phase, preparing.Attempt, preparing.BudgetRunes = "preparing", 1, 1000
	laterPending := preparing
	laterPending.Phase = "pending"
	superseded := preparing
	superseded.Phase = "superseded"
	provider, model, selected, changed := "b", "larger", int64(4), int64(3)
	superseded.SelectedProvider, superseded.SelectedModel = &provider, &model
	superseded.SelectedConfigSeq, superseded.ModelChangeSeq = &selected, &changed
	for _, tc := range []struct {
		name string
		p    contextRecoveryPayload
	}{
		{"first pending", pending}, {"preparing", preparing},
		{"later pending", laterPending}, {"superseded", superseded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := recoveryRecord(t, store.KindContextRecovery, tc.p)
			got, err := decodeContextRecovery(r)
			if err != nil || !reflect.DeepEqual(got, tc.p) {
				t.Fatalf("round trip = %+v, %v; want %+v", got, err, tc.p)
			}
		})
	}
	base := recoveryRecord(t, store.KindContextRecovery, pending)
	for _, key := range []string{"round", "step", "provider", "model", "config_seq", "before_runes", "rejected_outbound_runes", "code", "reason"} {
		t.Run("missing "+key, func(t *testing.T) {
			_, err := decodeContextRecovery(mutateRecoveryJSON(t, base, key, nil))
			if !errors.Is(err, ErrIncompatibleLog) {
				t.Fatalf("missing %s: %v", key, err)
			}
		})
	}
	for _, tc := range []struct {
		name, key, value string
	}{
		{"unknown phase", "phase", `"future"`},
		{"bare ready", "phase", `"ready"`},
		{"negative config", "config_seq", `-2`},
		{"negative round", "round", `-1`},
		{"null model", "model", `null`},
		{"explicit zero attempt", "attempt", `0`},
		{"null attempt", "attempt", `null`},
		{"unknown field", "new_field", `1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeContextRecovery(mutateRecoveryJSON(t, base, tc.key, json.RawMessage(tc.value)))
			if !errors.Is(err, ErrIncompatibleLog) {
				t.Fatalf("%s: %v", tc.name, err)
			}
		})
	}
	for _, key := range []string{"selected_provider", "selected_model", "selected_config_seq", "model_change_seq"} {
		t.Run("superseded missing "+key, func(t *testing.T) {
			r := recoveryRecord(t, store.KindContextRecovery, superseded)
			_, err := decodeContextRecovery(mutateRecoveryJSON(t, r, key, nil))
			if !errors.Is(err, ErrIncompatibleLog) {
				t.Fatalf("missing %s: %v", key, err)
			}
		})
	}
	for _, schema := range []int{0, 1, 2, 3, 5} {
		t.Run("schema "+jsonNumber(schema), func(t *testing.T) {
			r := base
			r.Schema = schema
			if _, err := decodeContextRecovery(r); !errors.Is(err, ErrIncompatibleLog) {
				t.Fatalf("schema %d: %v", schema, err)
			}
		})
	}
}

func TestHistoryReplacedCodec(t *testing.T) {
	base := historyReplacedPayload{RunID: "r", Round: 0, OperationID: "op", History: []llm.Message{llm.UserMessage("small")}}
	for _, reason := range []string{"proactive_compaction", "overflow_compaction", "legacy_anchor"} {
		t.Run(reason, func(t *testing.T) {
			p := base
			p.Reason = reason
			if reason == "overflow_compaction" {
				p.EpisodeID, p.Attempt, p.BudgetRunes, p.BeforeRunes, p.AfterRunes = "episode", 1, 500, 2000, 100
			}
			r := recoveryRecord(t, store.KindHistoryReplaced, p)
			got, err := decodeHistoryReplaced(r)
			if err != nil || !reflect.DeepEqual(got, p) {
				t.Fatalf("round trip = %+v, %v; want %+v", got, err, p)
			}
			for _, key := range []string{"episode_id", "attempt", "budget_runes", "before_runes", "after_runes"} {
				t.Run(key, func(t *testing.T) {
					changed := mutateRecoveryJSON(t, r, key, nil)
					if reason != "overflow_compaction" {
						changed = mutateRecoveryJSON(t, r, key, json.RawMessage(`null`))
					}
					if _, err := decodeHistoryReplaced(changed); !errors.Is(err, ErrIncompatibleLog) {
						t.Fatalf("reason %s field %s: %v", reason, key, err)
					}
				})
			}
		})
	}
	base.Reason = "legacy_anchor"
	r := recoveryRecord(t, store.KindHistoryReplaced, base)
	for _, key := range []string{"round", "run_id", "operation_id", "history"} {
		t.Run("missing "+key, func(t *testing.T) {
			if _, err := decodeHistoryReplaced(mutateRecoveryJSON(t, r, key, nil)); !errors.Is(err, ErrIncompatibleLog) {
				t.Fatalf("missing %s: %v", key, err)
			}
		})
	}
	for _, schema := range []int{0, 1, 2, 3, 5} {
		t.Run("schema "+jsonNumber(schema), func(t *testing.T) {
			r := r
			r.Schema = schema
			if _, err := decodeHistoryReplaced(r); !errors.Is(err, ErrIncompatibleLog) {
				t.Fatalf("schema %d: %v", schema, err)
			}
		})
	}
	if _, err := decodeHistoryReplaced(mutateRecoveryJSON(t, r, "reason", json.RawMessage(`"future"`))); !errors.Is(err, ErrIncompatibleLog) {
		t.Fatalf("unknown reason: %v", err)
	}
}
