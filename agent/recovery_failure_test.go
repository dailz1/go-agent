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

type recoveryFailStore struct {
	store.Store
	phase  string
	commit bool
	err    error
	failed bool
}

func (s *recoveryFailStore) Append(ctx context.Context, thread string, head int64, records ...store.Record) (int64, error) {
	for _, record := range records {
		phase := record.Kind
		if record.Kind == store.KindContextRecovery {
			payload, err := decodeContextRecovery(record)
			if err != nil {
				return 0, err
			}
			phase = payload.Phase
			if payload.Reason == "provider_error" {
				phase = payload.Reason
			}
		}
		if phase == s.phase && !s.failed {
			s.failed = true
			if s.commit {
				if _, err := s.Store.Append(ctx, thread, head, records...); err != nil {
					return 0, err
				}
			}
			return 0, s.err
		}
	}
	return s.Store.Append(ctx, thread, head, records...)
}

func TestRecoveryUnconfirmedWritesStopCostlyActions(t *testing.T) {
	for _, phase := range []string{"pending", "preparing", store.KindHistoryReplaced, "dispatched"} {
		for _, commit := range []bool{false, true} {
			name := phase
			if commit {
				name += "/acknowledgement lost"
			}
			t.Run(name, func(t *testing.T) {
				history := []llm.Message{llm.UserMessage(strings.Repeat("x", 2000))}
				short := []llm.Message{llm.UserMessage("summary")}
				disk := errors.New("injected storage failure")
				st := &recoveryFailStore{Store: store.NewMemory(), phase: phase, commit: commit, err: disk}
				p := agenttest.NewScriptedProvider(recoveryExchange(history, overflowTestError(), nil))
				compactions := 0
				a := New(p, tool.NewRegistry(), WithStore(st), WithLogger(discardLogger()),
					WithCompactor(CompactFunc(func(context.Context, []llm.Message, CompactionBudget, llm.Provider, []llm.Option) (CompactionResult, error) {
						compactions++
						return CompactionResult{History: short, Changed: true}, nil
					})))
				seq, err := a.RunStream(t.Context(), messageString(history[0]), WithThread("recovery"))
				if err != nil {
					t.Fatal(err)
				}
				events := 0
				var got error
				for event, err := range seq {
					if err != nil {
						got = err
					}
					if _, ok := event.(CompactionEvent); ok {
						events++
					}
				}
				wantCompactions, wantEvents := 0, 0
				if phase == store.KindHistoryReplaced || phase == "dispatched" {
					wantCompactions = 1
				}
				if phase == "dispatched" {
					wantEvents = 1
				}
				if !errors.Is(got, disk) || compactions != wantCompactions || events != wantEvents {
					t.Fatalf("error %v, compactions %d want %d, events %d want %d", got, compactions, wantCompactions, events, wantEvents)
				}
				if phase == "pending" {
					var api *llm.APIError
					if !errors.Is(got, llm.ErrContextOverflow) || !errors.As(got, &api) || api.StatusCode != 400 {
						t.Fatalf("pending write failure lost provider rejection: %v", got)
					}
				}
				v := recoveryView(t, a)
				want := history
				if phase == "dispatched" || (phase == store.KindHistoryReplaced && commit) {
					want = short
				}
				if !reflect.DeepEqual(v.history, want) {
					t.Fatalf("replay does not match acknowledged/actual write: %+v", v.history)
				}
				if err := p.Verify(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestRecoveryCallbackRefillStopsDispatch(t *testing.T) {
	history := []llm.Message{llm.UserMessage(strings.Repeat("x", 1000))}
	short := []llm.Message{llm.UserMessage(strings.Repeat("s", 700))}
	p := agenttest.NewScriptedProvider(recoveryExchange(history, overflowTestError(), nil))
	a := New(p, tool.NewRegistry(), WithStore(store.NewMemory()), WithLogger(discardLogger()),
		WithCompactor(recoveryCompactor(short)),
		WithRoundContextProvider(func(_ context.Context, r RoundContextRequest) (string, error) {
			if r.Attempt == 1 {
				return "", nil
			}
			return strings.Repeat("overlay", 100), nil
		}))
	_, err := a.Run(t.Context(), messageString(history[0]), WithThread("recovery"))
	requireRecoveryError(t, err, "no_progress", 1)
	v := recoveryView(t, a)
	if !reflect.DeepEqual(v.history, short) || v.recovery.Phase != "blocked" {
		t.Fatalf("refill reverted history or entered dispatch: %+v", v)
	}
	if err := p.Verify(); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryCallbackErrorsRetainLastConfirmedPhase(t *testing.T) {
	for _, tc := range []struct {
		name        string
		failAttempt int
		wantPhase   string
		wantSlots   int
	}{
		{"initial callback", 1, "", 0},
		{"ready callback", 2, "ready", 1},
		{"retry callback", 3, "dispatched", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			history := []llm.Message{llm.UserMessage(strings.Repeat("x", 2000))}
			short := []llm.Message{llm.UserMessage("summary")}
			exchanges := []agenttest.Exchange{}
			if tc.failAttempt > 1 {
				exchanges = append(exchanges, recoveryExchange(history, overflowTestError(), nil))
			}
			if tc.failAttempt > 2 {
				exchanges = append(exchanges, recoveryExchange(short, &llm.APIError{StatusCode: 503}, nil))
			}
			p := agenttest.NewScriptedProvider(exchanges...)
			callbackErr := overflowTestError()
			a := New(p, tool.NewRegistry(), WithStore(store.NewMemory()), WithLogger(discardLogger()),
				WithCompactor(recoveryCompactor(short)),
				WithRetryConfig(AgentRetryConfig{MaxRetries: 1, BaseDelay: 1}),
				WithRoundContextProvider(func(_ context.Context, r RoundContextRequest) (string, error) {
					if r.Attempt == tc.failAttempt {
						return "", callbackErr
					}
					return "", nil
				}))
			_, err := a.Run(t.Context(), messageString(history[0]), WithThread("recovery"))
			var callback *RoundContextError
			if !errors.As(err, &callback) || !errors.Is(err, callbackErr) || errors.Is(err, ErrContextRecoveryFailed) {
				t.Fatalf("callback error incorrectly classified as provider overflow: %v", err)
			}
			v := recoveryView(t, a)
			if tc.wantPhase == "" {
				if v.recovery != nil {
					t.Fatal("local failure created episode")
				}
			} else if v.recovery.Phase != tc.wantPhase || v.recovery.Attempt != tc.wantSlots || v.recovery.Reason == "provider_error" {
				t.Fatalf("local failure changed durable phase: %+v", v.recovery)
			}
			if err := p.Verify(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRecoveryProviderOutcomeWriteFailurePreservesBothErrors(t *testing.T) {
	history := []llm.Message{llm.UserMessage(strings.Repeat("x", 2000))}
	short := []llm.Message{llm.UserMessage(strings.Repeat("s", 800))}
	after := []llm.Message{llm.UserMessage("summary")}
	providerErr, disk := &llm.APIError{StatusCode: 503}, errors.New("outcome write failed")
	st := &recoveryFailStore{Store: store.NewMemory(), phase: "provider_error", err: disk}
	p := agenttest.NewScriptedProvider(recoveryExchange(history, overflowTestError(), nil),
		recoveryExchange(short, providerErr, nil), recoveryExchange(after, nil, nil, &llm.TextDeltaChunk{Text: "done"}))
	a := New(p, tool.NewRegistry(), WithStore(st), WithLogger(discardLogger()), WithRetryConfig(AgentRetryConfig{MaxRetries: -1}),
		WithCompactor(recoveryCompactor(short, after)))
	_, err := a.Run(t.Context(), messageString(history[0]), WithThread("recovery"))
	if !errors.Is(err, providerErr) || !errors.Is(err, disk) || errors.Is(err, ErrContextRecoveryFailed) {
		t.Fatalf("outcome errors lost: %v", err)
	}
	v := recoveryView(t, a)
	if v.recovery.Phase != "dispatched" || v.recovery.Reason == "provider_error" {
		t.Fatalf("invented write acknowledgement: %+v", v.recovery)
	}
	if _, err := a.ResumeThread(t.Context(), "recovery"); err != nil {
		t.Fatal(err)
	}
	if err := p.Verify(); err != nil {
		t.Fatal(err)
	}
}
