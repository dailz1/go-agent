package agent

import (
	"bytes"
	"context"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

func TestContextRuntimeCarryAndReplacementLimits(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name        string
		changed     bool
		carry       int
		replacement int
		tokens      int64
		budget      int
		overflow    bool
	}{
		{"unchanged_no_relief", false, 0, 0, 7000, 3428, true},
		{"unchanged_token_carry", false, 1000, 0, 7000, 3428, false},
		{"unchanged_hard_window", false, 4000, 0, 10000, 2400, true},
		{"replacement_above_Q", true, 0, 2401, 10000, 2400, true},
		{"replacement_rune_carry", true, 100, 2500, 10000, 2400, false},
		{"replacement_fallback_limit", true, 0, 6001, 10000, 2400, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			history := []llm.Message{llm.UserMessage("q"), llm.AssistantMessage("")}
			overhead := estimateRunes(history)
			history[1] = llm.AssistantMessage(strings.Repeat("x", 4000-overhead))
			p := NewMockProvider()
			state := &contextRuntime{}
			state.activateMeasurement(requestMeasurement(p, nil, nil), &llm.Usage{InputTokens: tt.tokens}, history, nil)
			compactor := CompactFunc(func(_ context.Context, h []llm.Message, b CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
				if b.MaxRunes != tt.budget {
					t.Fatalf("budget = %d, want %d", b.MaxRunes, tt.budget)
				}
				if tt.changed {
					h[1] = llm.AssistantMessage(strings.Repeat("z", tt.replacement-overhead))
				}
				return CompactionResult{History: h, Changed: tt.changed}, nil
			})
			st := store.NewMemory()
			a := New(p, tool.NewRegistry(), WithStore(st), WithCompactor(compactor), WithLogger(discardLogger()))
			sess, _, err := a.openSession(t.Context(), st, "carry", "q", false, false)
			if err != nil {
				t.Fatal(err)
			}
			defer releaseThread(st, "carry")
			sess.carry, sess.context = tt.carry, state
			var yielded error
			got, ok := a.compactHistoryRound(t.Context(), history, p, nil, func(_ AgentEvent, err error) bool {
				yielded = err
				return true
			}, sess, 7500, state)
			if ok == tt.overflow || (tt.overflow && yielded == nil) {
				t.Fatalf("budget ok/error = %v/%v", ok, yielded)
			}
			adopted := tt.changed && !tt.overflow
			if (state.anchor == nil) != adopted {
				t.Fatalf("adopted=%v, retained measurement=%v", adopted, state.anchor != nil)
			}
			if tt.overflow && !reflect.DeepEqual(got, history) {
				t.Fatal("rejected compaction changed history")
			}
		})
	}
}

func TestContextRuntimeNoCompactorAddsNoGate(t *testing.T) {
	t.Parallel()
	p := NewMockStreamingProvider([][]llm.Chunk{
		contextChunks("measured", &llm.Usage{InputTokens: math.MaxInt64}), textChunks("done"),
	})
	var buf bytes.Buffer
	a := New(p, tool.NewRegistry(), WithCompactor(nil), WithMaxIter(2),
		WithFinishRound(contextContinue), WithLogger(contextTestLogger(&buf)))
	if _, err := a.Run(t.Context(), "q"); err != nil {
		t.Fatal(err)
	}
	records := contextBudgetRecords(t, &buf)
	if len(records) != 2 || records[1].Budget != 0 || !records[1].Valid {
		t.Fatalf("no-compactor decisions = %+v", records)
	}
}

func TestContextRuntimeInvocationStartsWithoutMeasurement(t *testing.T) {
	t.Parallel()
	for _, model := range []string{"", "explicit"} {
		t.Run("model_"+model, func(t *testing.T) {
			p := NewMockProvider(
				MsgWithUsageResponse(llm.AssistantMessage("measured"), &llm.Usage{InputTokens: 2000}),
				MsgResponse(llm.AssistantMessage("done")),
			)
			var buf bytes.Buffer
			a := New(p, tool.NewRegistry(), WithLLMOptions(llm.WithModel(model)), WithLogger(contextTestLogger(&buf)))
			first, err := a.Run(t.Context(), "q")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := a.RunWithHistory(t.Context(), first.History, "next"); err != nil {
				t.Fatal(err)
			}
			records := contextBudgetRecords(t, &buf)
			if len(records) != 2 || records[1].Valid || records[1].Estimate != estimateRunes(p.LastMessages) {
				t.Fatalf("measurement escaped its invocation: %+v", records)
			}
		})
	}
}
