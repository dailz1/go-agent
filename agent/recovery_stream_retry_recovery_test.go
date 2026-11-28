package agent

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"

	"github.com/dailz1/go-agent/agenttest"
	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

func TestNetworkRetryPreservesOverflowSlotsAndBudget(t *testing.T) {
	for _, tc := range []struct {
		name       string
		slots      int
		setupRetry bool
		exhaust    bool
		refill     bool
		maxRetries int
		wantPrefix string
		wantCalls  int
	}{
		{"first slot", 1, false, false, false, 1, "CR", 3},
		{"interleaved prefix", 1, true, false, false, 2, "RCR", 4},
		{"budget survives compaction", 1, true, true, false, 1, "RC", 3},
		{"last slot", 2, false, false, false, 1, "CCR", 4},
		{"last slot outbound refill", 2, false, false, true, 1, "CCR", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				original := []llm.Message{llm.UserMessage(strings.Repeat("x", 2000))}
				one := []llm.Message{llm.UserMessage(strings.Repeat("s", 800))}
				two := []llm.Message{llm.UserMessage(strings.Repeat("s", 300))}
				short := one
				var exchanges []agenttest.Exchange
				if tc.setupRetry {
					exchanges = append(exchanges, recoveryExchange(original, syscall.ECONNRESET, nil))
				}
				exchanges = append(exchanges, recoveryExchange(original, overflowTestError(), nil))
				if tc.slots == 2 {
					exchanges = append(exchanges, recoveryExchange(one, nil, overflowTestError()))
					short = two
				}
				exchanges = append(exchanges, recoveryExchange(short, nil, syscall.ECONNRESET))
				if !tc.exhaust && !tc.refill {
					exchanges = append(exchanges, recoveryExchange(short, nil, nil, &llm.TextDeltaChunk{Text: "done"}))
				}
				p := agenttest.NewScriptedProvider(exchanges...)
				st := store.NewMemory()
				compactions, requests := 0, 0
				callbacks := 0
				var a *Agent
				a = New(p, tool.NewRegistry(), WithStore(st), WithLogger(discardLogger()),
					WithCompactor(CompactFunc(func(ctx context.Context, _ []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
						compactions++
						if compactions == 1 {
							if _, err := a.EnqueueInputWithID(ctx, "recovery", "queued", "steering"); err != nil {
								t.Fatal(err)
							}
							return CompactionResult{History: one, Changed: true}, nil
						}
						return CompactionResult{History: two, Changed: true}, nil
					})),
					WithRoundContextProvider(func(_ context.Context, req RoundContextRequest) (string, error) {
						requests++
						if tc.refill && req.Attempt == 4 {
							return strings.Repeat("r", 1000), nil
						}
						return "", nil
					}),
					WithRetryConfig(AgentRetryConfig{MaxRetries: tc.maxRetries, OnRetry: func(RetryInfo) { callbacks++ }}))
				stream, err := a.RunStream(t.Context(), messageString(original[0]), WithThread("recovery"))
				if err != nil {
					t.Fatal(err)
				}
				var prefix string
				var got error
				var retries []RetryInfo
				var done *DoneEvent
				for event, err := range stream {
					switch e := event.(type) {
					case RetryEvent:
						prefix += "R"
						retries = append(retries, e.RetryInfo)
						if compactions > 0 {
							v := recoveryView(t, a)
							if len(v.queue) != 1 || v.recovery == nil || v.recovery.Reason != "request" {
								t.Fatalf("intermediate retry consumed steering or wrote provider_error: %+v", v)
							}
						}
					case CompactionEvent:
						prefix += "C"
					case DoneEvent:
						done = &e
					}
					if err != nil {
						got = err
					}
				}
				if compactions != tc.slots || prefix != tc.wantPrefix || callbacks != len(retries) {
					t.Fatalf("compactions=%d prefix=%s callbacks=%d retries=%+v", compactions, prefix, callbacks, retries)
				}
				wantRequests := tc.wantCalls
				if tc.refill {
					wantRequests++ // Callback runs before the outbound progress check.
				}
				if requests != wantRequests {
					t.Fatalf("request preparations=%d want=%d", requests, wantRequests)
				}
				for i, info := range retries {
					if info.Attempt != i+2 || info.MaxAttempts != tc.maxRetries+1 {
						t.Fatalf("retry budget reset: %+v", info)
					}
				}
				switch {
				case tc.refill:
					requireRecoveryError(t, got, "no_progress", 2)
				case tc.exhaust:
					if !errors.Is(got, syscall.ECONNRESET) || done != nil {
						t.Fatalf("budget exhaustion = %v done=%+v", got, done)
					}
					v := recoveryView(t, a)
					if v.recovery == nil || v.recovery.Reason != "provider_error" {
						t.Fatalf("terminal failure not persisted: %+v", v.recovery)
					}
				default:
					if got != nil || done == nil || messageString(done.Message) != "done" || len(done.Retries) != len(retries) {
						t.Fatalf("result=%+v error=%v", done, got)
					}
				}
				if len(recoveryView(t, a).queue) != 1 {
					t.Fatal("network retry drained queued steering")
				}
				if err := p.Verify(); err != nil {
					t.Fatal(err)
				}
			})
		})
	}
}

func TestNetworkRetryDefersSteeringUntilNextRound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		original := []llm.Message{llm.UserMessage(strings.Repeat("x", 2000))}
		short := []llm.Message{llm.UserMessage("summary")}
		next := append(deepCopyMessages(short), llm.AssistantMessage("continue"), llm.UserMessage("queued"))
		p := agenttest.NewScriptedProvider(recoveryExchange(original, overflowTestError(), nil),
			recoveryExchange(short, nil, syscall.ECONNRESET),
			recoveryExchange(short, nil, nil, &llm.TextDeltaChunk{Text: "continue"}),
			recoveryExchange(next, nil, nil, &llm.TextDeltaChunk{Text: "done"}))
		var a *Agent
		compactions := 0
		a = New(p, tool.NewRegistry(), WithStore(store.NewMemory()), WithLogger(discardLogger()),
			WithCompactor(CompactFunc(func(ctx context.Context, _ []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
				compactions++
				if _, err := a.EnqueueInputWithID(ctx, "recovery", "q", "queued"); err != nil {
					t.Fatal(err)
				}
				return CompactionResult{History: short, Changed: true}, nil
			})),
			WithFinishRound(func(_ context.Context, fc FinishRoundContext) (FinishRoundDecision, error) {
				if fc.Round == 0 {
					return FinishRoundDecision{Action: FinishRoundContinue}, nil
				}
				return FinishRoundDecision{}, nil
			}))
		result, err := a.Run(t.Context(), messageString(original[0]), WithThread("recovery"))
		if err != nil || compactions != 1 {
			t.Fatalf("result=%+v err=%v compactions=%d", result, err, compactions)
		}
		v := recoveryView(t, a)
		if len(v.queue) != 0 || !reflect.DeepEqual(result.History, append(next, llm.AssistantMessage("done"))) {
			t.Fatalf("next round did not consume steering: %+v", v)
		}
		if err := p.Verify(); err != nil {
			t.Fatal(err)
		}
	})
}
