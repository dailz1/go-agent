package agent

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/agenttest"
	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

func overflowTestError() error {
	return &llm.ContextOverflowError{Code: "context_length_exceeded", Err: &llm.APIError{StatusCode: 400}}
}

func recoveryExchange(history []llm.Message, outer, stream error, chunks ...llm.Chunk) agenttest.Exchange {
	return agenttest.Exchange{
		Method:         agenttest.MethodChatStream,
		Request:        agenttest.Request{Messages: history, Tools: []tool.ToolInfo{}, Options: llm.ApplyOptions(nil)},
		StreamOuterErr: outer, StreamErr: stream, StreamChunks: chunks,
	}
}

func recoveryCompactor(outputs ...[]llm.Message) Compactor {
	next := 0
	return CompactFunc(func(context.Context, []llm.Message, CompactionBudget, llm.Provider, []llm.Option) (CompactionResult, error) {
		if next == len(outputs) {
			return CompactionResult{}, errors.New("unexpected compaction")
		}
		history := outputs[next]
		next++
		return CompactionResult{History: history, Changed: true}, nil
	})
}

func recoveryView(t *testing.T, a *Agent) threadView {
	t.Helper()
	v, err := a.replayThread(t.Context(), "recovery")
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func requireRecoveryError(t *testing.T, err error, reason string, attempts int) {
	t.Helper()
	var recovery *ContextRecoveryError
	if !errors.As(err, &recovery) || !errors.Is(err, ErrContextRecoveryFailed) ||
		!errors.Is(err, llm.ErrContextOverflow) || recovery.Reason != reason || recovery.Attempts != attempts {
		t.Fatalf("error = %v, want recovery %s after %d slots", err, reason, attempts)
	}
}

func TestRecoveryBudgetArithmetic(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		w, target, h, previous, want int
	}{
		{"history below target", 32000, 25600, 18000, 0, 13500},
		{"second uses adopted history", 32000, 25600, 12800, 13500, 9600},
		{"previous budget binds", 32000, 25600, 18000, 13500, 10125},
		{"window binds", 800, 1000, 1200, 0, 600},
		{"round down", 7, 7, 7, 0, 5},
		{"exhausted", 1, 1, 1, 0, 0},
		{"maximum integer", math.MaxInt, math.MaxInt, math.MaxInt, 0, math.MaxInt/4*3 + 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := recoveryBudget(tc.w, tc.target, tc.h, tc.previous); got != tc.want {
				t.Fatalf("budget = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestRecoveryZeroChunkShrinksBeforeResend(t *testing.T) {
	for _, mode := range []string{"outer", "iterator", "ephemeral"} {
		t.Run(mode, func(t *testing.T) {
			history := []llm.Message{llm.UserMessage(strings.Repeat("x", 2000))}
			short := []llm.Message{llm.UserMessage(strings.Repeat("s", 800))}
			overflow := overflowTestError()
			first := recoveryExchange(history, overflow, nil)
			if mode != "outer" {
				first = recoveryExchange(history, nil, overflow)
			}
			provider := agenttest.NewScriptedProvider(first,
				recoveryExchange(short, nil, nil, &llm.TextDeltaChunk{Text: "done"}, &llm.DoneChunk{}))
			compactions := 0
			compactor := CompactFunc(func(_ context.Context, input []llm.Message, b CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
				compactions++
				if b.MaxRunes != estimateRunes(history)*3/4 || b.MaxRunes >= estimateRunes(input) {
					t.Fatalf("ineffective target %d", b.MaxRunes)
				}
				input[0] = llm.UserMessage("mutated input")
				return CompactionResult{Changed: true, History: short}, nil
			})
			a := New(provider, tool.NewRegistry(), WithStore(store.NewMemory()),
				WithCompactor(compactor), WithLogger(discardLogger()))
			options := []RunOption{WithThread("recovery")}
			if mode == "ephemeral" {
				options = nil
			}
			seq, err := a.RunStream(t.Context(), messageString(history[0]), options...)
			if err != nil {
				t.Fatal(err)
			}
			var kinds []string
			for event, err := range seq {
				if err != nil {
					t.Fatalf("error yielded before recovery succeeded: %v", err)
				}
				switch e := event.(type) {
				case CompactionEvent:
					kinds = append(kinds, "compaction")
					if !reflect.DeepEqual(e.History, short) {
						t.Fatal("wrong adopted history")
					}
				case TextDeltaEvent:
					kinds = append(kinds, "text")
				case DoneEvent:
					kinds = append(kinds, "done")
				default:
					t.Fatalf("unexpected event %T", event)
				}
			}
			if !reflect.DeepEqual(kinds, []string{"compaction", "text", "done"}) || compactions != 1 {
				t.Fatalf("events %v, compactions %d", kinds, compactions)
			}
			if err := provider.Verify(); err != nil {
				t.Fatal(err)
			}
			if mode != "ephemeral" {
				v := recoveryView(t, a)
				if v.recovery != nil || v.runActive || messageString(v.history[0]) != messageString(short[0]) {
					t.Fatalf("success did not commit replacement and clear recovery: %+v", v)
				}
			}
		})
	}
}

func TestRecoveryInvalidCompactionNeverAdopted(t *testing.T) {
	input := strings.Repeat("x", 2000)
	original := []llm.Message{llm.SystemMessage("fixed"), llm.UserMessage(input)}
	for _, tc := range []struct {
		name   string
		result CompactionResult
		err    error
		reason string
	}{
		{"unchanged", CompactionResult{History: original}, nil, "no_progress"},
		{"same size", CompactionResult{History: original, Changed: true}, nil, "no_progress"},
		{"larger", CompactionResult{History: append(deepCopyMessages(original), llm.UserMessage("extra")), Changed: true}, nil, "no_progress"},
		{"empty", CompactionResult{Changed: true}, nil, "invalid_compaction"},
		{"system changed", CompactionResult{History: []llm.Message{llm.UserMessage("short")}, Changed: true}, nil, "invalid_compaction"},
		{"over budget", CompactionResult{History: []llm.Message{llm.SystemMessage("fixed"), llm.UserMessage(strings.Repeat("s", 1800))}, Changed: true}, nil, "budget_exceeded"},
		{"summarizer overflow", CompactionResult{}, overflowTestError(), "compactor_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			overflow := overflowTestError()
			provider := agenttest.NewScriptedProvider(recoveryExchange(original, overflow, nil))
			calls := 0
			a := New(provider, tool.NewRegistry(), WithSystemPrompt("fixed"), WithStore(store.NewMemory()),
				WithLogger(discardLogger()), WithCompactor(CompactFunc(func(_ context.Context, h []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
					calls++
					h[1] = llm.UserMessage("mutated by compactor")
					return tc.result, tc.err
				})))
			_, err := a.Run(t.Context(), input, WithThread("recovery"))
			requireRecoveryError(t, err, tc.reason, 1)
			v := recoveryView(t, a)
			if calls != 1 || !reflect.DeepEqual(seededHistory(v.system, v.history), original) || v.recovery.Phase != "blocked" {
				t.Fatalf("invalid result changed history or retried: %+v, calls %d", v, calls)
			}
			if !errors.Is(err, overflow) || (tc.err != nil && !errors.Is(err, tc.err)) {
				t.Fatal("original failure chain lost")
			}
			if err := provider.Verify(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRecoveryExhaustionSurvivesResume(t *testing.T) {
	original := []llm.Message{llm.UserMessage(strings.Repeat("x", 2000))}
	one := []llm.Message{llm.UserMessage(strings.Repeat("s", 800))}
	two := []llm.Message{llm.UserMessage(strings.Repeat("s", 300))}
	provider := agenttest.NewScriptedProvider(
		recoveryExchange(original, overflowTestError(), nil),
		recoveryExchange(one, nil, overflowTestError()),
		recoveryExchange(two, overflowTestError(), nil))
	a := New(provider, tool.NewRegistry(), WithStore(store.NewMemory()), WithLogger(discardLogger()),
		WithCompactor(recoveryCompactor(one, two)))
	_, err := a.Run(t.Context(), messageString(original[0]), WithThread("recovery"))
	requireRecoveryError(t, err, "attempts_exhausted", 2)
	_, err = a.ResumeThread(t.Context(), "recovery")
	requireRecoveryError(t, err, "attempts_exhausted", 2)
	v := recoveryView(t, a)
	if v.recovery.Attempt != 2 || v.recovery.Phase != "blocked" || !reflect.DeepEqual(v.history, two) {
		t.Fatalf("exhaustion reset: %+v", v)
	}
	if err := provider.Verify(); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryTypedNilCompactionBlocksInvalidWireForm(t *testing.T) {
	original := []llm.Message{llm.UserMessage(strings.Repeat("x", 2000))}
	p := agenttest.NewScriptedProvider(recoveryExchange(original, overflowTestError(), nil))
	var block *llm.TextBlock
	calls := 0
	a := New(p, tool.NewRegistry(), WithStore(store.NewMemory()), WithLogger(discardLogger()),
		WithCompactor(CompactFunc(func(context.Context, []llm.Message, CompactionBudget, llm.Provider, []llm.Option) (CompactionResult, error) {
			calls++
			return CompactionResult{Changed: true, History: []llm.Message{
				{Role: llm.RoleUser, Content: []llm.ContentBlock{block}},
			}}, nil
		})))
	_, err := a.Run(t.Context(), messageString(original[0]), WithThread("recovery"))
	requireRecoveryError(t, err, "invalid_compaction", 1)
	if errors.Is(err, ErrIncompatibleLog) {
		t.Fatalf("invalid compactor result misreported as log corruption: %v", err)
	}
	before := recoveryView(t, a)
	if before.recovery.Phase != "blocked" || !reflect.DeepEqual(before.history, original) {
		t.Fatalf("invalid output stranded or adopted: %+v", before)
	}
	_, err = a.ResumeThread(t.Context(), "recovery")
	requireRecoveryError(t, err, "invalid_compaction", 1)
	if calls != 1 || recoveryView(t, a).head != before.head {
		t.Fatal("Resume spent another slot on invalid compaction")
	}
}
