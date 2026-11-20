package agent

import (
	"bytes"
	"errors"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
)

func TestMeasurementInterruptionEstimateParity(t *testing.T) {
	for _, backend := range []string{"memory", "jsonl"} {
		for _, system := range []string{"", "frozen system"} {
			for _, tc := range []struct {
				name       string
				kind       string
				after      bool
				breakEvent string
				realPrefix int
				repeat     bool
			}{
				{name: "before_response_record_lands", kind: store.KindRoundDeclared, repeat: true},
				{name: "ambiguous_response_record_write", kind: store.KindRoundDeclared, after: true},
				{name: "open_declaration_before_any_result", breakEvent: "call"},
				{name: "after_some_round_results", breakEvent: "result", realPrefix: 1},
				{name: "after_round_commit", kind: store.KindRoundCommitted, after: true, realPrefix: 2},
				{name: "before_natural_done_record_lands", kind: store.KindAgentEvent, realPrefix: 2},
			} {
				t.Run(backend+"/"+system+"/"+tc.name, func(t *testing.T) {
					steps := [][]llm.Chunk{
						measurementToolChunks(&llm.Usage{InputTokens: 2000, OutputTokens: 200}),
						textChunks("done"),
					}
					var control bytes.Buffer
					baseline := measurementTestAgent(store.NewMemory(), measurementRegistry(tc.realPrefix),
						steps, &control, WithSystemPrompt(system))
					if _, err := baseline.Run(t.Context(), "question", WithThread("control")); err != nil {
						t.Fatal(err)
					}
					want := contextBudgetRecords(t, &control)
					if len(want) != 2 || !want[1].Valid {
						t.Fatalf("uninterrupted measurement missing: %+v", want)
					}
					st, reopen := measurementBackend(t, backend)
					fault := &measurementFaultStore{Store: st, kind: tc.kind, after: tc.after}
					var interrupted bytes.Buffer
					a := measurementTestAgent(fault, measurementRegistry(tc.realPrefix), steps, &interrupted,
						WithSystemPrompt(system))
					seq, err := a.RunStream(t.Context(), "question", WithThread("thread"))
					if err != nil {
						t.Fatal(err)
					}
					var runErr error
					for event, err := range seq {
						if err != nil {
							runErr = err
							break
						}
						if _, ok := event.(ToolCallEvent); ok && tc.breakEvent == "call" {
							break
						}
						if _, ok := event.(ToolResultEvent); ok && tc.breakEvent == "result" {
							break
						}
					}
					if tc.kind != "" && !errors.Is(runErr, errMeasurementCrash) {
						t.Fatalf("fault was not exercised: %v", runErr)
					}
					resumeSteps := steps[1:]
					expected := want[1:]
					if tc.repeat {
						resumeSteps, expected = steps, want
					}
					var resumed bytes.Buffer
					a = measurementTestAgent(reopen(), measurementRegistry(tc.realPrefix), resumeSteps, &resumed,
						WithSystemPrompt("changed host system"))
					if _, err := a.ResumeThread(t.Context(), "thread"); err != nil {
						t.Fatal(err)
					}
					got := contextBudgetRecords(t, &resumed)
					if len(got) != len(expected) {
						t.Fatalf("resumed decisions %d, want %d", len(got), len(expected))
					}
					for i := range got {
						assertMeasurementBudget(t, got[i], expected[i])
					}
				})
			}
		}
	}
}
