package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

func TestContextRuntimeConfigIdentity(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		changes []ConfigChange
		keep    bool
	}{
		{"same_pair", []ConfigChange{{Provider: strptr("a"), Model: strptr("x")}}, true},
		{"same_model", []ConfigChange{{Model: strptr("x")}}, true},
		{"provider_switch", []ConfigChange{{Provider: strptr("b"), Model: strptr("y")}}, false},
		{"model_switch", []ConfigChange{{Model: strptr("y")}}, false},
		{"round_trip_without_request", []ConfigChange{{Model: strptr("y")}, {Model: strptr("x")}}, false},
		{"same_thinking_override", []ConfigChange{{Thinking: strptr("enabled")}}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st := store.NewMemory()
			if tt.name == "same_pair" {
				// This case tests a repeated explicit route. The first
				// default-to-named switch is a distinct identity change.
				if err := SetThreadConfig(t.Context(), st, "thread", ConfigChange{
					Provider: strptr("a"), Model: strptr("x"),
				}); err != nil {
					t.Fatal(err)
				}
			}
			aProvider := &windowedProvider{name: "a", steps: [][]llm.Chunk{
				contextChunks("measured", &llm.Usage{InputTokens: 2000, OutputTokens: 200}), textChunks("done"),
			}}
			bProvider := &windowedProvider{name: "b", steps: [][]llm.Chunk{textChunks("done")}}
			var buf bytes.Buffer
			a := New(aProvider, tool.NewRegistry(), WithStore(st), WithProviders(bProvider),
				WithLLMOptions(llm.WithModel("x"), llm.WithThinking("enabled")),
				WithMaxIter(2), WithLogger(contextTestLogger(&buf)),
				WithFinishRound(func(ctx context.Context, _ FinishRoundContext) (FinishRoundDecision, error) {
					for _, change := range tt.changes {
						if err := SetThreadConfig(ctx, st, "thread", change); err != nil {
							return FinishRoundDecision{}, err
						}
					}
					return FinishRoundDecision{Action: FinishRoundContinue}, nil
				}))
			if _, err := a.Run(t.Context(), "q", WithThread("thread")); err != nil {
				t.Fatal(err)
			}
			records := contextBudgetRecords(t, &buf)
			if len(records) != 2 || records[1].Valid != tt.keep {
				t.Fatalf("identity decisions = %+v", records)
			}
			var sent []llm.Message
			if len(bProvider.received) > 0 {
				sent = bProvider.received[0]
			} else {
				sent = aProvider.received[1]
			}
			want := estimateRunes(sent)
			if tt.keep {
				want = 2200
			}
			if records[1].Estimate != want {
				t.Fatalf("estimate = %d, want %d", records[1].Estimate, want)
			}
		})
	}
}

func TestContextRuntimeFingerprintChanges(t *testing.T) {
	t.Parallel()
	for _, changeTools := range []bool{false, true} {
		t.Run(map[bool]string{false: "effective_options", true: "dynamic_tools"}[changeTools], func(t *testing.T) {
			registry := tool.NewRegistry()
			p := NewMockStreamingProvider([][]llm.Chunk{
				contextChunks("measured", &llm.Usage{InputTokens: 2000}), textChunks("done"),
			})
			seed := int64(1)
			var buf bytes.Buffer
			a := New(p, registry, WithDynamicTools(), WithMaxIter(2),
				WithLLMOptions(func(o *llm.Options) { n := seed; o.Seed = &n }),
				WithLogger(contextTestLogger(&buf)),
				WithFinishRound(func(context.Context, FinishRoundContext) (FinishRoundDecision, error) {
					seed++
					if changeTools {
						seed-- // isolate the tool fingerprint from option changes
						registry.MustRegister(&mockTool{info: tool.ToolInfo{Name: "new_tool"}})
					}
					return FinishRoundDecision{Action: FinishRoundContinue}, nil
				}))
			if _, err := a.Run(t.Context(), "q"); err != nil {
				t.Fatal(err)
			}
			records := contextBudgetRecords(t, &buf)
			if len(records) != 2 || records[1].Valid || records[1].Reason != "fingerprint_changed" ||
				records[1].Estimate != estimateRunes(p.AllMessages[1]) {
				t.Fatalf("fingerprint decisions = %+v", records)
			}
		})
	}
}

func TestContextRuntimeFallbackCapturesFrozenAttempt(t *testing.T) {
	t.Parallel()
	for _, change := range []bool{false, true} {
		t.Run(map[bool]string{false: "chat_usage_captured", true: "attempt_identity_frozen"}[change], func(t *testing.T) {
			p := NewMockProvider(
				MsgWithUsageResponse(llm.AssistantMessage("measured"), &llm.Usage{InputTokens: 2000, OutputTokens: 200}),
				MsgResponse(llm.AssistantMessage("done")),
			)
			model := "original"
			var buf bytes.Buffer
			var attempts []int
			a := New(p, tool.NewRegistry(), WithMaxIter(2),
				WithLLMOptions(func(o *llm.Options) { o.Model = model }),
				WithRoundContextProvider(func(_ context.Context, req RoundContextRequest) (string, error) {
					attempts = append(attempts, req.Attempt)
					if change {
						model = "mutated-after-freeze"
					}
					return "", nil
				}), WithFinishRound(contextContinue), WithLogger(contextTestLogger(&buf)))
			if _, err := a.Run(t.Context(), "q"); err != nil {
				t.Fatal(err)
			}
			records := contextBudgetRecords(t, &buf)
			if len(records) != 2 || records[1].Valid == change || len(attempts) != 4 || attempts[1] != 2 {
				t.Fatalf("fallback budget decisions = %+v; attempts %v", records, attempts)
			}
			if change && records[1].Reason != "model_changed" {
				t.Fatalf("fallback lost the frozen request identity: %+v", records)
			}
			if !change && records[1].Estimate != 2200 {
				t.Fatalf("fallback usage = %+v", records)
			}
		})
	}
}

func TestContextRuntimeSteeringMeasuredCapacity(t *testing.T) {
	t.Parallel()
	for _, inputTokens := range []int64{100, 1950} {
		t.Run(map[int64]string{100: "measured_space_drains", 1950: "measured_full_defers"}[inputTokens], func(t *testing.T) {
			st := store.NewMemory()
			registry := tool.NewRegistry()
			var a *Agent
			registry.MustRegister(mutatorTool("enqueue", func(ctx context.Context, _ json.RawMessage) (*tool.ToolResult, error) {
				_, err := a.EnqueueInputWithID(ctx, "thread", "queued", "steering accepted")
				return tool.NewTextResult("result"), err
			}))
			first := toolCallChunks("one", "enqueue")
			first[len(first)-1] = &llm.DoneChunk{Usage: &llm.Usage{InputTokens: inputTokens}}
			p := NewMockStreamingProvider([][]llm.Chunk{first, textChunks("done")})
			a = New(p, registry, WithStore(st), WithCompactor(nil), WithContextWindowTokens(2000), WithLogger(discardLogger()))
			if _, err := a.Run(t.Context(), "q", WithThread("thread")); err != nil {
				t.Fatal(err)
			}
			received := historyContainsText(p.AllMessages[1], "steering accepted")
			if received != (inputTokens == 100) {
				t.Fatalf("steering drained = %v for usage %d", received, inputTokens)
			}
		})
	}
}

func TestContextRuntimeConfigAbsorbedDuringDeclaration(t *testing.T) {
	t.Parallel()
	for _, thinking := range []bool{false, true} {
		t.Run(map[bool]string{false: "model_round_trip", true: "same_thinking"}[thinking], func(t *testing.T) {
			st := &contextAbsorbingStore{Store: store.NewMemory()}
			st.inject = func(ctx context.Context) error {
				changes := []ConfigChange{{Model: strptr("y")}, {Model: strptr("x")}}
				if thinking {
					changes = []ConfigChange{{Thinking: strptr("enabled")}}
				}
				for _, change := range changes {
					if err := SetThreadConfig(ctx, st, "thread", change); err != nil {
						return err
					}
				}
				return nil
			}
			p := NewMockStreamingProvider([][]llm.Chunk{
				contextChunks("measured", &llm.Usage{InputTokens: 2000}), textChunks("done"),
			})
			var buf bytes.Buffer
			a := New(p, tool.NewRegistry(), WithStore(st), WithMaxIter(2),
				WithLLMOptions(llm.WithModel("x"), llm.WithThinking("enabled")),
				WithFinishRound(contextContinue), WithLogger(contextTestLogger(&buf)))
			if _, err := a.Run(t.Context(), "q", WithThread("thread")); err != nil {
				t.Fatal(err)
			}
			records := contextBudgetRecords(t, &buf)
			if len(records) != 2 || records[1].Valid || records[1].Estimate != estimateRunes(p.AllMessages[1]) {
				t.Fatalf("absorbed control resurrected pending measurement: %+v", records)
			}
		})
	}
}

type contextAbsorbingStore struct {
	store.Store
	inject func(context.Context) error
}

func (s *contextAbsorbingStore) Append(ctx context.Context, thread string, head int64, records ...store.Record) (int64, error) {
	if s.inject != nil && records[0].Kind == store.KindRoundDeclared {
		inject := s.inject
		s.inject = nil
		if err := inject(ctx); err != nil {
			return 0, err
		}
	}
	return s.Store.Append(ctx, thread, head, records...)
}
