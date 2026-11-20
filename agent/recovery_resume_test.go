package agent

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dailz1/go-agent/agenttest"
	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

func TestRecoveryBreakKeepsReadyForResume(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
		window  int
		reason  string
	}{
		{"nil compactor reuses ready", true, 8192, ""},
		{"disabled blocks ready", false, 8192, "disabled"},
		{"smaller window needs compactor", true, 500, "unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := []llm.Message{llm.UserMessage(strings.Repeat("x", 2000))}
			short := []llm.Message{llm.UserMessage(strings.Repeat("s", 800))}
			st := store.NewMemory()
			first := agenttest.NewScriptedProvider(recoveryExchange(original, overflowTestError(), nil))
			a := New(first, tool.NewRegistry(), WithStore(st), WithLogger(discardLogger()),
				WithCompactor(recoveryCompactor(short)))
			seq, err := a.RunStream(t.Context(), messageString(original[0]), WithThread("recovery"))
			if err != nil {
				t.Fatal(err)
			}
			saw := false
			for event, err := range seq {
				if err != nil {
					t.Fatal(err)
				}
				if _, ok := event.(CompactionEvent); ok {
					saw = true
					break
				}
			}
			v := recoveryView(t, a)
			if !saw || v.recovery.Phase != "ready" || v.recovery.Attempt != 1 || !reflect.DeepEqual(v.history, short) {
				t.Fatalf("break did not retain ready: %+v", v)
			}
			if err := first.Verify(); err != nil {
				t.Fatal(err)
			}
			exchanges := []agenttest.Exchange{}
			if tc.reason == "" {
				exchanges = append(exchanges, recoveryExchange(short, nil, nil, llm.TextDeltaChunk{Text: "done"}))
			}
			next := agenttest.NewScriptedProvider(exchanges...)
			a = New(next, tool.NewRegistry(), WithStore(st), WithLogger(discardLogger()), WithCompactor(nil),
				WithOverflowRecovery(tc.enabled), WithContextWindowTokens(tc.window))
			result, err := a.ResumeThread(t.Context(), "recovery")
			if tc.reason != "" {
				requireRecoveryError(t, err, tc.reason, 1)
				if got := recoveryView(t, a); !reflect.DeepEqual(got.recovery, v.recovery) {
					t.Fatalf("disabled/unavailable changed persisted state: %+v", got.recovery)
				}
			} else if err != nil || messageString(result.Message) != "done" {
				t.Fatalf("ready Resume = %+v, %v", result, err)
			}
			if err := next.Verify(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRecoveryPendingSwitchesDoNotOccupySlot(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "unavailable"}[enabled], func(t *testing.T) {
			original := []llm.Message{llm.UserMessage(strings.Repeat("x", 2000))}
			st := store.NewMemory()
			p := agenttest.NewScriptedProvider(recoveryExchange(original, overflowTestError(), nil))
			a := New(p, tool.NewRegistry(), WithStore(st), WithLogger(discardLogger()),
				WithOverflowRecovery(enabled), WithCompactor(nil))
			_, err := a.Run(t.Context(), messageString(original[0]), WithThread("recovery"))
			reason := "disabled"
			if enabled {
				reason = "unavailable"
			}
			requireRecoveryError(t, err, reason, 0)
			before := recoveryView(t, a)
			if before.recovery.Phase != "pending" || before.recovery.Attempt != 0 {
				t.Fatalf("slot occupied without enabled compactor: %+v", before.recovery)
			}
			_, err = a.ResumeThread(t.Context(), "recovery")
			requireRecoveryError(t, err, reason, 0)
			if err := p.Verify(); err != nil {
				t.Fatal(err)
			}
			short := []llm.Message{llm.UserMessage("summary")}
			next := agenttest.NewScriptedProvider(recoveryExchange(short, nil, nil, llm.TextDeltaChunk{Text: "done"}))
			a = New(next, tool.NewRegistry(), WithStore(st), WithLogger(discardLogger()),
				WithCompactor(recoveryCompactor(short)))
			if _, err := a.ResumeThread(t.Context(), "recovery"); err != nil {
				t.Fatal(err)
			}
			if err := next.Verify(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRecoveryProviderFailureUsesRemainingSlotOnlyOnResume(t *testing.T) {
	history := []llm.Message{llm.UserMessage(strings.Repeat("x", 2000))}
	one := []llm.Message{llm.UserMessage(strings.Repeat("s", 800))}
	two := []llm.Message{llm.UserMessage(strings.Repeat("s", 300))}
	unavailable := &llm.APIError{StatusCode: 503, Body: "temporary"}
	p := agenttest.NewScriptedProvider(
		recoveryExchange(history, overflowTestError(), nil),
		recoveryExchange(one, unavailable, nil),
		recoveryExchange(one, unavailable, nil),
		recoveryExchange(two, nil, nil, llm.TextDeltaChunk{Text: "done"}))
	var budgets []int
	var attempts []int
	a := New(p, tool.NewRegistry(), WithStore(store.NewMemory()), WithLogger(discardLogger()),
		WithRetryConfig(AgentRetryConfig{MaxRetries: 1, BaseDelay: time.Nanosecond}),
		WithRoundContextProvider(func(_ context.Context, req RoundContextRequest) (string, error) {
			attempts = append(attempts, req.Attempt)
			return "", nil
		}),
		WithCompactor(CompactFunc(func(_ context.Context, _ []llm.Message, b CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
			budgets = append(budgets, b.MaxRunes)
			h := one
			if len(budgets) == 2 {
				h = two
			}
			return CompactionResult{History: h, Changed: true}, nil
		})))
	_, err := a.Run(t.Context(), messageString(history[0]), WithThread("recovery"))
	if !errors.Is(err, unavailable) || errors.Is(err, ErrContextRecoveryFailed) || len(budgets) != 1 {
		t.Fatalf("ordinary failure was reclassified or compacted immediately: %v, budgets %v", err, budgets)
	}
	v := recoveryView(t, a)
	if v.recovery.Phase != "dispatched" || v.recovery.Reason != "provider_error" || v.recovery.Attempt != 1 {
		t.Fatalf("provider failure outcome = %+v", v.recovery)
	}
	if _, err := a.ResumeThread(t.Context(), "recovery"); err != nil {
		t.Fatal(err)
	}
	want := []int{estimateRunes(history) * 3 / 4, estimateRunes(one) * 3 / 4}
	if !reflect.DeepEqual(budgets, want) || !reflect.DeepEqual(attempts, []int{1, 2, 3, 1}) {
		t.Fatalf("budgets %v want %v; attempts %v", budgets, want, attempts)
	}
	records, err := a.store.History(t.Context(), "recovery", 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, record := range records {
		if record.Kind == store.KindContextRecovery {
			payload, err := decodeContextRecovery(record)
			if err != nil {
				t.Fatal(err)
			}
			if payload.Phase == "preparing" && payload.Attempt == 2 {
				found = payload.Reason == "resume_after_provider_error"
			}
		}
	}
	if !found {
		t.Fatal("Resume did not record ordinary provider failure as the reason")
	}
	if err := p.Verify(); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryResidualSlotsAreNotReused(t *testing.T) {
	for _, tc := range []struct {
		name   string
		prefix int
		budget int
	}{
		{"preparing", 3, 900},
		{"dispatched", 5, estimateRunes([]llm.Message{llm.UserMessage(strings.Repeat("s", 600))}) * 3 / 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := store.NewMemory()
			log := recoveryLogFixture(t)[:tc.prefix]
			for i, record := range log {
				if record.Kind != store.KindContextRecovery {
					continue
				}
				payload, err := decodeContextRecovery(record)
				if err != nil {
					t.Fatal(err)
				}
				payload.Provider, payload.Model = "agenttest_scripted", ""
				log[i] = mustRecord(t, record.Kind, record.ID, payload)
				log[i].Schema = store.SchemaV4
			}
			if _, err := st.Append(t.Context(), "recovery", 0, log...); err != nil {
				t.Fatal(err)
			}
			short := []llm.Message{llm.UserMessage("summary")}
			p := agenttest.NewScriptedProvider(recoveryExchange(short, nil, nil, llm.TextDeltaChunk{Text: "done"}))
			calls := 0
			a := New(p, tool.NewRegistry(), WithStore(st), WithLogger(discardLogger()),
				WithCompactor(CompactFunc(func(_ context.Context, _ []llm.Message, b CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
					calls++
					if b.MaxRunes != tc.budget {
						t.Fatalf("budget %d want %d", b.MaxRunes, tc.budget)
					}
					return CompactionResult{History: short, Changed: true}, nil
				})))
			if _, err := a.ResumeThread(t.Context(), "recovery"); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("compactions %d", calls)
			}
			if err := p.Verify(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRecoveryEmptyContinuedTurnResumesAfterOverflow(t *testing.T) {
	original := []llm.Message{llm.UserMessage(strings.Repeat("x", 2000))}
	continued := append(deepCopyMessages(original), llm.Message{Role: llm.RoleAssistant})
	short := []llm.Message{llm.UserMessage("summary")}
	p := agenttest.NewScriptedProvider(
		recoveryExchange(original, nil, nil, llm.DoneChunk{}),
		recoveryExchange(continued, nil, overflowTestError(), llm.TextDeltaChunk{Text: "partial"}),
		recoveryExchange(short, nil, nil, llm.TextDeltaChunk{Text: "done"}))
	a := New(p, tool.NewRegistry(), WithStore(store.NewMemory()), WithLogger(discardLogger()),
		WithCompactor(recoveryCompactor(short)),
		WithFinishRound(func(context.Context, FinishRoundContext) (FinishRoundDecision, error) {
			return FinishRoundDecision{Action: FinishRoundContinue}, nil
		}))
	_, err := a.Run(t.Context(), messageString(original[0]), WithThread("recovery"))
	if !errors.Is(err, llm.ErrContextOverflow) {
		t.Fatalf("expected overflow after empty continued turn: %v", err)
	}
	a.finishRound = nil
	result, err := a.ResumeThread(t.Context(), "recovery")
	if err != nil || messageString(result.Message) != "done" {
		t.Fatalf("empty continued turn wedged Resume: %+v, %v", result, err)
	}
	if err := p.Verify(); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryResumeTinyWindowBlocksWithoutConsumingSlot(t *testing.T) {
	original := []llm.Message{llm.UserMessage(strings.Repeat("x", 2000))}
	short := []llm.Message{llm.UserMessage(strings.Repeat("s", 800))}
	providerErr := errors.New("ordinary provider failure")
	p := agenttest.NewScriptedProvider(recoveryExchange(original, overflowTestError(), nil),
		recoveryExchange(short, providerErr, nil))
	st := store.NewMemory()
	a := New(p, tool.NewRegistry(), WithStore(st), WithLogger(discardLogger()),
		WithCompactor(recoveryCompactor(short)))
	if _, err := a.Run(t.Context(), messageString(original[0]), WithThread("recovery")); !errors.Is(err, providerErr) {
		t.Fatal(err)
	}
	before := recoveryView(t, a)
	next := agenttest.NewScriptedProvider()
	compactions := 0
	a = New(next, tool.NewRegistry(), WithStore(st), WithLogger(discardLogger()), WithContextWindowTokens(1),
		WithCompactor(CompactFunc(func(context.Context, []llm.Message, CompactionBudget, llm.Provider, []llm.Option) (CompactionResult, error) {
			compactions++
			return CompactionResult{}, errors.New("must not compact")
		})))
	_, err := a.ResumeThread(t.Context(), "recovery")
	requireRecoveryError(t, err, "budget_exhausted", 1)
	if errors.Is(err, ErrIncompatibleLog) {
		t.Fatalf("legal budget stop rejected: %v", err)
	}
	after := recoveryView(t, a)
	if compactions != 0 || after.recovery.Phase != "blocked" || after.recovery.Reason != "budget_exhausted" ||
		after.recovery.Attempt != 1 || after.recovery.BudgetRunes != before.recovery.BudgetRunes ||
		!reflect.DeepEqual(after.history, before.history) {
		t.Fatalf("budget stop changed occupied slot or history: %+v", after)
	}
	_, err = a.ResumeThread(t.Context(), "recovery")
	requireRecoveryError(t, err, "budget_exhausted", 1)
	if err := next.Verify(); err != nil {
		t.Fatal(err)
	}
}
