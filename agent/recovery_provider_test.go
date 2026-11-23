package agent

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/agenttest"
	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

func TestRecoveryOverflowPrecedesRetryAndFallback(t *testing.T) {
	for _, extra := range []struct {
		name string
		err  error
	}{
		{"retryable", &llm.APIError{StatusCode: 503}},
		{"fallback", llm.ErrStreamingNotSupported},
		{"both", errors.Join(&llm.APIError{StatusCode: 503}, llm.ErrStreamingNotSupported)},
	} {
		t.Run(extra.name, func(t *testing.T) {
			history := []llm.Message{llm.UserMessage(strings.Repeat("x", 2000))}
			short := []llm.Message{llm.UserMessage("summary")}
			p := agenttest.NewScriptedProvider(
				recoveryExchange(history, errors.Join(overflowTestError(), extra.err), nil),
				recoveryExchange(short, nil, nil, &llm.TextDeltaChunk{Text: "done"}))
			a := New(p, tool.NewRegistry(), WithLogger(discardLogger()), WithCompactor(recoveryCompactor(short)))
			result, err := a.Run(t.Context(), messageString(history[0]))
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Retries) != 0 {
				t.Fatalf("overflow entered transport retry: %+v", result.Retries)
			}
			if err := p.Verify(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRecoveryFallbackMaintainsRoundAttemptCounter(t *testing.T) {
	history := []llm.Message{llm.UserMessage(strings.Repeat("x", 2000))}
	short := []llm.Message{llm.UserMessage("summary")}
	chat := func(h []llm.Message, err error) *agenttest.Exchange {
		e := &agenttest.Exchange{Method: agenttest.MethodChat,
			Request: agenttest.Request{Messages: h, Tools: []tool.ToolInfo{}, Options: llm.ApplyOptions(nil)}, ChatErr: err}
		if err == nil {
			m := llm.AssistantMessage("done")
			e.ChatResponse = &m
		}
		return e
	}
	p := agenttest.NewScriptedProvider(recoveryExchange(history, llm.ErrStreamingNotSupported, nil), *chat(history, overflowTestError()),
		recoveryExchange(short, llm.ErrStreamingNotSupported, nil), *chat(short, nil))
	var attempts []int
	a := New(p, tool.NewRegistry(), WithLogger(discardLogger()), WithCompactor(recoveryCompactor(short)),
		WithRoundContextProvider(func(_ context.Context, r RoundContextRequest) (string, error) {
			attempts = append(attempts, r.Attempt)
			return "", nil
		}))
	if _, err := a.Run(t.Context(), messageString(history[0])); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(attempts, []int{1, 2, 3, 4}) {
		t.Fatalf("attempts reset across fallback/recovery: %v", attempts)
	}
	if err := p.Verify(); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverySecondSlotOrdinaryFailureBlocksOnlyNextResume(t *testing.T) {
	history := []llm.Message{llm.UserMessage(strings.Repeat("x", 2000))}
	one := []llm.Message{llm.UserMessage(strings.Repeat("s", 800))}
	two := []llm.Message{llm.UserMessage(strings.Repeat("s", 300))}
	failure := &llm.APIError{StatusCode: 503}
	p := agenttest.NewScriptedProvider(recoveryExchange(history, overflowTestError(), nil),
		recoveryExchange(one, overflowTestError(), nil), recoveryExchange(two, failure, nil))
	st := store.NewMemory()
	a := New(p, tool.NewRegistry(), WithStore(st), WithLogger(discardLogger()), WithCompactor(recoveryCompactor(one, two)),
		WithRetryConfig(AgentRetryConfig{MaxRetries: -1}))
	_, err := a.Run(t.Context(), messageString(history[0]), WithThread("recovery"))
	if !errors.Is(err, failure) || errors.Is(err, ErrContextRecoveryFailed) {
		t.Fatalf("ordinary failure replaced: %v", err)
	}
	v := recoveryView(t, a)
	if v.recovery.Phase != "dispatched" || v.recovery.Reason != "provider_error" || v.recovery.Attempt != 2 {
		t.Fatalf("failure blocked current call: %+v", v.recovery)
	}
	a = New(p, tool.NewRegistry(), WithStore(st), WithLogger(discardLogger()), WithCompactor(nil))
	_, err = a.ResumeThread(t.Context(), "recovery")
	requireRecoveryError(t, err, "attempts_exhausted", 2)
	if err := p.Verify(); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverySecondRejectionClearsOptionalCode(t *testing.T) {
	history := []llm.Message{llm.UserMessage(strings.Repeat("x", 2000))}
	short := []llm.Message{llm.UserMessage(strings.Repeat("s", 800))}
	p := agenttest.NewScriptedProvider(recoveryExchange(history, overflowTestError(), nil),
		recoveryExchange(short, nil, llm.ErrContextOverflow, &llm.TextDeltaChunk{Text: "partial"}))
	a := New(p, tool.NewRegistry(), WithStore(store.NewMemory()), WithLogger(discardLogger()),
		WithCompactor(recoveryCompactor(short)))
	if _, err := a.Run(t.Context(), messageString(history[0]), WithThread("recovery")); !errors.Is(err, llm.ErrContextOverflow) {
		t.Fatalf("second rejection = %v", err)
	}
	v := recoveryView(t, a)
	if v.recovery == nil || v.recovery.Code != "" || v.recovery.Phase != "pending" || v.recovery.Attempt != 1 {
		t.Fatalf("second pending inherited stale rejection code: %+v", v.recovery)
	}
	if err := p.Verify(); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryResumeSkipsProactiveCompactionAndDrain(t *testing.T) {
	history := []llm.Message{llm.UserMessage(strings.Repeat("x", 2000))}
	short := []llm.Message{llm.UserMessage("summary")}
	after := append(deepCopyMessages(short), llm.AssistantMessage("continue"), llm.UserMessage("queued"))
	p := agenttest.NewScriptedProvider(recoveryExchange(history, nil, overflowTestError(), &llm.TextDeltaChunk{Text: "partial"}),
		recoveryExchange(short, nil, nil, &llm.TextDeltaChunk{Text: "continue"}),
		recoveryExchange(after, nil, nil, &llm.TextDeltaChunk{Text: "done"}))
	st := store.NewMemory()
	budgets := []int{}
	a := New(p, tool.NewRegistry(), WithStore(st), WithLogger(discardLogger()),
		WithCompactor(CompactFunc(func(_ context.Context, _ []llm.Message, b CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
			budgets = append(budgets, b.MaxRunes)
			return CompactionResult{History: short, Changed: true}, nil
		})),
		WithFinishRound(func(_ context.Context, fc FinishRoundContext) (FinishRoundDecision, error) {
			if fc.Round == 0 {
				return FinishRoundDecision{Action: FinishRoundContinue}, nil
			}
			return FinishRoundDecision{}, nil
		}))
	if _, err := a.Run(t.Context(), messageString(history[0]), WithThread("recovery")); !errors.Is(err, llm.ErrContextOverflow) {
		t.Fatal(err)
	}
	if _, err := a.EnqueueInputWithID(t.Context(), "recovery", "q", "queued"); err != nil {
		t.Fatal(err)
	}
	result, err := a.ResumeThread(t.Context(), "recovery")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(budgets, []int{estimateRunes(history) * 3 / 4}) || messageString(result.Message) != "done" {
		t.Fatalf("Resume did not force recovery exactly once: budgets %v, result %+v", budgets, result)
	}
	if err := p.Verify(); err != nil {
		t.Fatal(err)
	}
}
