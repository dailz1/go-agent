package agent

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

func TestMeasurementMissingUsageResumeParity(t *testing.T) {
	for _, backend := range []string{"memory", "jsonl"} {
		for _, tc := range []struct {
			name  string
			usage *llm.Usage
		}{
			{"nil_usage_after_valid_measurement", nil},
			{"zero_usage_after_valid_measurement", &llm.Usage{}},
			{"output_only_after_valid_measurement", &llm.Usage{OutputTokens: 30}},
			{"invalid_usage_after_valid_measurement", &llm.Usage{InputTokens: 10, CachedTokens: 11}},
		} {
			t.Run(backend+"/"+tc.name, func(t *testing.T) {
				steps := [][]llm.Chunk{
					contextChunks("measured", &llm.Usage{InputTokens: 2000, OutputTokens: 200}),
					{&llm.DoneChunk{Usage: tc.usage}}, // nil Content on the live empty assistant
					textChunks("done"),
				}
				var control bytes.Buffer
				base := measurementTestAgent(store.NewMemory(), measurementRegistry(0), steps, &control,
					WithFinishRound(contextContinue), WithMaxIter(3))
				if _, err := base.Run(t.Context(), "q", WithThread("control")); err != nil {
					t.Fatal(err)
				}
				want := contextBudgetRecords(t, &control)
				if len(want) != 3 || !want[2].Valid || want[2].Tail == 0 {
					t.Fatalf("older measured boundary was not preserved: %+v", want)
				}
				st, reopen := measurementBackend(t, backend)
				var interrupted bytes.Buffer
				a := measurementTestAgent(st, measurementRegistry(0), steps, &interrupted,
					WithMaxIter(3), WithFinishRound(func(_ context.Context, f FinishRoundContext) (FinishRoundDecision, error) {
						if f.Round == 1 {
							return FinishRoundDecision{}, errMeasurementCrash
						}
						return FinishRoundDecision{Action: FinishRoundContinue}, nil
					}))
				if _, err := a.Run(t.Context(), "q", WithThread("thread")); !errors.Is(err, errMeasurementCrash) {
					t.Fatalf("terminal_text hook failure: %v", err)
				}
				var resumed bytes.Buffer
				a = measurementTestAgent(reopen(), measurementRegistry(0), steps[2:], &resumed)
				if _, err := a.ResumeThread(t.Context(), "thread"); err != nil {
					t.Fatal(err)
				}
				got := contextBudgetRecords(t, &resumed)
				if len(got) != 1 {
					t.Fatalf("resumed decisions: %+v", got)
				}
				assertMeasurementBudget(t, got[0], want[2])
			})
		}
	}
}

func TestMeasurementTerminalBoundaryOnNewRun(t *testing.T) {
	for _, backend := range []string{"memory", "jsonl"} {
		for _, mode := range []string{"natural_done_new_run", "cooperative_close", "max_iteration_stop"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
				st, reopen := measurementBackend(t, backend)
				response := contextChunks("measured", &llm.Usage{InputTokens: 2000, OutputTokens: 200})
				options := []Option{WithSystemPrompt("frozen")}
				switch mode {
				case "cooperative_close":
					response = measurementToolChunks(&llm.Usage{InputTokens: 2000, OutputTokens: 200})
					options = append(options, WithFinishRound(func(context.Context, FinishRoundContext) (FinishRoundDecision, error) {
						return FinishRoundDecision{Action: FinishRoundEnd}, nil
					}))
				case "max_iteration_stop":
					response = measurementToolChunks(&llm.Usage{InputTokens: 2000, OutputTokens: 200})
					options = append(options, WithMaxIter(1))
				}
				var first bytes.Buffer
				a := measurementTestAgent(st, measurementRegistry(2), [][]llm.Chunk{response}, &first, options...)
				result, err := a.Run(t.Context(), "q", WithThread("thread"))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := a.ResumeThread(t.Context(), "thread"); !errors.Is(err, ErrNothingToResume) {
					t.Fatalf("terminal record left active run: %v", err)
				}
				// The measured prefix is system + user + assistant, regardless
				// of terminal results or cumulative spending.
				want := 2200 + appendRunesDelta(result.History[:3],
					append(deepCopyMessages(result.History[3:]), llm.UserMessage("next")))
				var next bytes.Buffer
				a = measurementTestAgent(reopen(), measurementRegistry(2), [][]llm.Chunk{textChunks("done")}, &next)
				if _, err := a.Run(t.Context(), "next", WithThread("thread")); err != nil {
					t.Fatal(err)
				}
				got := contextBudgetRecords(t, &next)
				if len(got) != 1 || !got[0].Valid || got[0].Estimate != want || got[0].AnchorTokens != 2200 {
					t.Fatalf("terminal boundary moved across results/new input: %+v, want %d", got, want)
				}
			})
		}
	}
}

func TestMeasurementResumeRechecksLiveIdentity(t *testing.T) {
	for _, mode := range []string{
		"same_identity", "dynamic_tools", "changed_options", "empty_model_no_restore",
		"run_with_history_no_anchor", "prior_overlay_not_recovered",
	} {
		t.Run(mode, func(t *testing.T) {
			st := store.NewMemory()
			reg := measurementRegistry(0)
			var first bytes.Buffer
			options := []Option{WithFinishRound(func(context.Context, FinishRoundContext) (FinishRoundDecision, error) {
				return FinishRoundDecision{}, errMeasurementCrash
			})}
			if mode == "empty_model_no_restore" {
				options = append(options, WithLLMOptions(llm.WithModel("")))
			}
			if mode == "prior_overlay_not_recovered" {
				options = append(options, WithRoundContextProvider(func(context.Context, RoundContextRequest) (string, error) {
					return "old transient overlay marker", nil
				}))
			}
			a := measurementTestAgent(st, reg, [][]llm.Chunk{contextChunks("measured", &llm.Usage{InputTokens: 2000})}, &first, options...)
			if _, err := a.Run(t.Context(), "q", WithThread("thread")); !errors.Is(err, errMeasurementCrash) {
				t.Fatal(err)
			}
			v, err := a.replayThread(t.Context(), "thread")
			if err != nil {
				t.Fatal(err)
			}
			options = nil
			switch mode {
			case "dynamic_tools":
				reg.MustRegister(&mockTool{info: tool.ToolInfo{Name: "new_tool"}})
				options = append(options, WithDynamicTools())
			case "changed_options":
				options = append(options, WithLLMOptions(llm.WithModel("x"), llm.WithTemperature(0.2)))
			case "empty_model_no_restore":
				options = append(options, WithLLMOptions(llm.WithModel("")))
			case "prior_overlay_not_recovered":
				options = append(options, WithRoundContextProvider(func(context.Context, RoundContextRequest) (string, error) {
					return "new snapshot", nil
				}))
			}
			var resumed bytes.Buffer
			a = measurementTestAgent(st, reg, [][]llm.Chunk{textChunks("done")}, &resumed, options...)
			if mode == "run_with_history_no_anchor" {
				_, err = a.RunWithHistory(t.Context(), seededHistory(v.system, v.history), "edited")
			} else {
				_, err = a.ResumeThread(t.Context(), "thread")
			}
			if err != nil {
				t.Fatal(err)
			}
			got := contextBudgetRecords(t, &resumed)
			valid := mode == "same_identity" || mode == "prior_overlay_not_recovered"
			if len(got) != 1 || got[0].Valid != valid {
				t.Fatalf("resume identity decision: %+v", got)
			}
			if valid && got[0].Estimate != 2000 {
				t.Fatalf("restored estimate = %+v", got[0])
			}
			if !valid {
				history := seededHistory(v.system, v.history)
				if mode == "run_with_history_no_anchor" {
					history = append(history, llm.UserMessage("edited"))
				}
				if got[0].Estimate != estimateRunes(history) {
					t.Fatalf("identity mismatch reused anchor: %+v", got[0])
				}
			}
			if mode == "prior_overlay_not_recovered" {
				for _, record := range logRecords(t, st, "thread") {
					if bytes.Contains(record.Payload, []byte("old transient overlay marker")) {
						t.Fatal("overlay leaked into durable history")
					}
				}
			}
		})
	}
}
