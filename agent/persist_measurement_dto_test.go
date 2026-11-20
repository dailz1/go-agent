package agent

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
)

func TestMeasurementDeclarationCodecAndBoundary(t *testing.T) {
	base := measurementRecord(t, 0, llm.AssistantMessage("answer"), &llm.Usage{InputTokens: 100}, 2)
	for _, tc := range []struct {
		name, key, value string
	}{
		{"missing_provider", "provider", ""},
		{"missing_model", "model", ""},
		{"missing_fingerprint", "request_fingerprint", ""},
		{"missing_version", "fingerprint_version", ""},
		{"missing_prefix", "prefix_messages", ""},
		{"missing_usage", "usage", ""},
		{"missing_capture_config_seq", "capture_config_seq", ""},
		{"null_capture_config_seq", "capture_config_seq", "null"},
		{"null_provider", "provider", "null"},
		{"non_string_provider", "provider", `42`},
		{"invalid_fingerprint", "request_fingerprint", `"xyz"`},
		{"uppercase_fingerprint", "request_fingerprint", `"` + strings.Repeat("A", 64) + `"`},
		{"future_fingerprint", "fingerprint_version", "2"},
		{"zero_prefix", "prefix_messages", "0"},
		{"corrupt_prefix_messages", "prefix_messages", "3"},
		{"unknown_field", "extra", "true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := measurementJSON(base.Payload)
			var value json.RawMessage
			if tc.value != "" {
				value = json.RawMessage(tc.value)
			}
			nested := mutateRecoveryJSON(t, store.Record{Payload: raw}, tc.key, value)
			record := mutateRecoveryJSON(t, base, "context_measurement", nested.Payload)
			log := sequenceRecoveryLog([]store.Record{
				mustRecord(t, store.KindRunStarted, "r-start", runStartedPayload{RunID: "r", Input: "q"}),
				record,
			})
			if _, err := replayRecords(threadView{lastRound: -1, cfgSeq: -1}, log); !errors.Is(err, ErrIncompatibleLog) {
				t.Fatalf("corrupt measurement accepted: %v", err)
			}
		})
	}
	for _, value := range []json.RawMessage{nil, json.RawMessage("null")} {
		r := mutateRecoveryJSON(t, base, "context_measurement", value)
		if _, err := decodeRoundDeclared(r); !errors.Is(err, ErrIncompatibleLog) {
			t.Fatalf("missing/null descriptor accepted: %v", err)
		}
	}
}

func TestMeasurementCaptureSequenceMismatchDiscardsUsage(t *testing.T) {
	for _, sequence := range []string{"-2", "0", "9223372036854775807"} {
		t.Run(sequence, func(t *testing.T) {
			r := measurementRecord(t, 0, llm.AssistantMessage("answer"), &llm.Usage{InputTokens: 2000}, 2)
			raw := mutateRecoveryJSON(t, store.Record{Payload: measurementJSON(r.Payload)},
				"capture_config_seq", json.RawMessage(sequence))
			r = mutateRecoveryJSON(t, r, "context_measurement", raw.Payload)
			v := measurementView(t, []store.Record{
				mustRecord(t, store.KindRunStarted, "r-start", runStartedPayload{RunID: "r", Input: "q"}), r,
			})
			if v.anchor != nil || measurementEstimate(v) != estimateRunes(v.history) ||
				!historyContainsText(v.history, "answer") {
				t.Fatalf("capture sequence mismatch activated usage or lost the response: %+v", v)
			}
		})
	}
}

func TestMeasurementDoneUsageProvenance(t *testing.T) {
	identity := measurementIdentityFor(requestMeasurement(NewMockStreamingProvider(nil),
		[]llm.Option{llm.WithModel("x")}, nil), 2)
	for _, tc := range []struct {
		name      string
		reported  bool
		usage     llm.Usage
		truncated bool
		null      bool
		reject    bool
	}{
		{name: "absent_usage"},
		{name: "reported_zero", reported: true},
		{name: "reported_usage", reported: true, usage: llm.Usage{InputTokens: 100}},
		{name: "unreported_nonzero", usage: llm.Usage{OutputTokens: 1}, reject: true},
		{name: "nontruncated_null_measurement", null: true, reject: true},
		{name: "truncated_null_measurement", truncated: true, null: true},
		{name: "truncated_repeated_measurement", truncated: true, reject: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &doneMeasurement{measurementIdentity: identity, UsageReported: tc.reported, CaptureConfigSeq: -1}
			if tc.null {
				m = nil
			}
			payload := doneEventPayload{
				Message: llm.AssistantMessage("answer"), Usage: tc.usage,
				Truncated: tc.truncated, ContextMeasurement: m,
			}
			r := mustRecord(t, store.KindAgentEvent, "r-done", eventEnvelope{
				Type: "done", Version: eventEnvelopeV1, Payload: mustJSON(t, payload),
			})
			r.Schema = store.SchemaV5
			_, err := decodeAgentEvent(r)
			if tc.reject != errors.Is(err, ErrIncompatibleLog) {
				t.Fatalf("done provenance: %v", err)
			}
			if !tc.null && !tc.truncated {
				for _, key := range []string{"usage_reported", "capture_config_seq"} {
					raw := mutateRecoveryJSON(t, store.Record{Payload: mustJSON(t, m)}, key, nil)
					inner := mutateRecoveryJSON(t, store.Record{Payload: mustJSON(t, payload)}, "context_measurement", raw.Payload)
					bad := mutateRecoveryJSON(t, r, "payload", inner.Payload)
					if _, err := decodeAgentEvent(bad); !errors.Is(err, ErrIncompatibleLog) {
						t.Fatalf("done missing %s accepted: %v", key, err)
					}
				}
			}
		})
	}
}

func TestMeasurementMixedResponses(t *testing.T) {
	for _, tc := range []struct {
		name    string
		schema  int
		usage   *llm.Usage
		nullFP  bool
		changed bool
		keep    bool
	}{
		{"new_nil_preserves_boundary", 5, nil, false, false, true},
		{"new_zero_preserves_boundary", 5, &llm.Usage{}, false, false, true},
		{"null_fingerprint_clears_boundary", 5, &llm.Usage{InputTokens: 500}, true, false, false},
		{"different_identity_without_usage", 5, nil, false, true, false},
		{"old_v1_response", 1, nil, false, false, false},
		{"old_v2_response", 2, nil, false, false, false},
		{"old_v3_response", 3, nil, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			second := measurementRecord(t, 1, llm.AssistantMessage("suffix"), tc.usage, 3)
			second.Schema = tc.schema
			if tc.schema < 5 {
				second = mutateRecoveryJSON(t, second, "context_measurement", nil)
			} else {
				if tc.nullFP {
					nested := mutateRecoveryJSON(t, store.Record{Payload: measurementJSON(second.Payload)}, "request_fingerprint", []byte("null"))
					second = mutateRecoveryJSON(t, second, "context_measurement", nested.Payload)
				}
				if tc.changed {
					nested := mutateRecoveryJSON(t, store.Record{Payload: measurementJSON(second.Payload)}, "model", []byte(`"y"`))
					second = mutateRecoveryJSON(t, second, "context_measurement", nested.Payload)
				}
			}
			v := measurementView(t, []store.Record{
				mustRecord(t, store.KindRunStarted, "r-start", runStartedPayload{RunID: "r", Input: "q"}),
				measurementRecord(t, 0, llm.AssistantMessage("measured"), &llm.Usage{InputTokens: 2000}, 2),
				second,
			})
			want := estimateRunes(v.history)
			if tc.keep {
				want = 2000 + appendRunesDelta(v.history[:2], v.history[2:])
			}
			if got := measurementEstimate(v); got != want {
				t.Fatalf("mixed log estimate = %d, want %d", got, want)
			}
		})
	}
}
