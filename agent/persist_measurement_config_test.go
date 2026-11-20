package agent

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
)

func TestMeasurementConfigAbsorptionReplayParity(t *testing.T) {
	for _, tc := range []struct {
		name    string
		changes []ConfigChange
		valid   bool
	}{
		{"A_B_A_before_declaration_append", []ConfigChange{{Model: strptr("y")}, {Model: strptr("x")}}, false},
		{"same_value_Thinking_before_append", []ConfigChange{{Thinking: strptr("enabled")}}, false},
		{"same_value_model_before_append", []ConfigChange{{Model: strptr("x")}}, false},
		{"hot_switch_during_response_persistence", []ConfigChange{{Model: strptr("y")}}, false},
		{"genuine_new_A_request", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var want contextBudgetRecord
			for _, interrupted := range []bool{false, true} {
				st := &contextAbsorbingStore{Store: store.NewMemory()}
				st.inject = func(ctx context.Context) error {
					for _, change := range tc.changes {
						if err := SetThreadConfig(ctx, st, "thread", change); err != nil {
							return err
						}
					}
					return nil
				}
				var buf bytes.Buffer
				a := measurementTestAgent(st, measurementRegistry(0), [][]llm.Chunk{
					contextChunks("measured", &llm.Usage{InputTokens: 2000}), textChunks("done"),
				}, &buf, WithMaxIter(2),
					WithLLMOptions(llm.WithModel("x"), llm.WithThinking("enabled")),
					WithFinishRound(func(context.Context, FinishRoundContext) (FinishRoundDecision, error) {
						if interrupted {
							return FinishRoundDecision{}, errMeasurementCrash
						}
						return FinishRoundDecision{Action: FinishRoundContinue}, nil
					}))
				_, err := a.Run(t.Context(), "q", WithThread("thread"))
				if interrupted != errors.Is(err, errMeasurementCrash) {
					t.Fatalf("interrupted %v: %v", interrupted, err)
				}
				for _, r := range logRecords(t, st, "thread") {
					if r.Kind != store.KindRoundDeclared {
						continue
					}
					p, err := decodeRoundDeclared(r)
					if err != nil {
						t.Fatal(err)
					}
					if p.ContextMeasurement.Model != "x" || p.ContextMeasurement.CaptureConfigSeq != -1 {
						t.Fatalf("response lost pre-absorb identity/config sequence: %+v", p.ContextMeasurement)
					}
				}
				if !interrupted {
					records := contextBudgetRecords(t, &buf)
					if len(records) != 2 || records[1].Valid != tc.valid {
						t.Fatalf("live absorption decision: %+v", records)
					}
					want = records[1]
					continue
				}
				var resumed bytes.Buffer
				a = measurementTestAgent(st, measurementRegistry(0), [][]llm.Chunk{textChunks("done")}, &resumed,
					WithLLMOptions(llm.WithModel("x"), llm.WithThinking("enabled")))
				if _, err := a.ResumeThread(t.Context(), "thread"); err != nil {
					t.Fatal(err)
				}
				records := contextBudgetRecords(t, &resumed)
				if len(records) != 1 {
					t.Fatalf("resume decisions: %+v", records)
				}
				assertMeasurementBudget(t, records[0], want)
			}
		})
	}
}

func TestMeasurementPendingConfigInvalidation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		changes []configChangedPayload
		valid   bool
	}{
		{"same_model_keeps_pending", []configChangedPayload{{Model: strptr("x")}}, true},
		{"model_round_trip_clears_pending", []configChangedPayload{{Model: strptr("y")}, {Model: strptr("x")}}, false},
		{"thinking_clears_pending", []configChangedPayload{{Thinking: strptr("enabled")}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := []store.Record{
				mustRecord(t, store.KindRunStarted, "r-start", runStartedPayload{RunID: "r", Input: "q"}),
				measurementRecord(t, 0, toolCallMessage("one"), &llm.Usage{InputTokens: 2000}, 2),
			}
			for i, change := range tc.changes {
				r := mustRecord(t, store.KindConfigChanged, "change"+jsonNumber(i), change)
				r.Schema = store.SchemaV2
				log = append(log, r)
			}
			v := measurementView(t, log)
			v.closeUnknown()
			if (v.anchor != nil) != tc.valid {
				t.Fatalf("pending config resurrected boundary: %+v", v.anchor)
			}
			expected := estimateRunes(v.history)
			if tc.valid {
				expected = 2000 + appendRunesDelta(v.history[:2], v.history[2:])
			}
			if got := measurementEstimate(v); got != expected {
				t.Fatalf("pending estimate = %d, want %d", got, expected)
			}
		})
	}
}

func TestMeasurementLaterRequestCanEstablishAfterSwitch(t *testing.T) {
	st := store.NewMemory()
	var buf bytes.Buffer
	a := measurementTestAgent(st, measurementRegistry(0), [][]llm.Chunk{
		contextChunks("A", &llm.Usage{InputTokens: 2000}),
		contextChunks("new A", &llm.Usage{InputTokens: 3000}),
		textChunks("done"),
	}, &buf, WithMaxIter(3), WithFinishRound(func(ctx context.Context, f FinishRoundContext) (FinishRoundDecision, error) {
		if f.Round == 0 {
			for _, model := range []string{"y", "x"} {
				if err := SetThreadConfig(ctx, st, "thread", ConfigChange{Model: &model}); err != nil {
					return FinishRoundDecision{}, err
				}
			}
		}
		return FinishRoundDecision{Action: FinishRoundContinue}, nil
	}))
	if _, err := a.Run(t.Context(), "q", WithThread("thread")); err != nil {
		t.Fatal(err)
	}
	records := contextBudgetRecords(t, &buf)
	v, err := a.replayThread(t.Context(), "thread")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 3 || records[1].Valid || !records[2].Valid || records[2].Estimate != 3000 {
		t.Fatalf("new request failed to establish after durable switch: %+v", records)
	}
	want := 3000 + appendRunesDelta(v.history[:3], v.history[3:])
	if got := measurementEstimate(v); got != want {
		t.Fatalf("latest boundary replay estimate = %d, want %d", got, want)
	}
}
