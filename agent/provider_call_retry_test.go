package agent

import (
	"context"
	"errors"
	"reflect"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/dailz1/go-agent/agenttest"
	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

func TestZeroDeliveryRetryCancellationAndBreak(t *testing.T) {
	for _, stage := range []string{"before retry", "event break", "callback cancel", "sleep cancel", "completed stream cancel"} {
		t.Run(stage, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				history := []llm.Message{llm.UserMessage("input")}
				first := recoveryExchange(history, nil, syscall.ECONNRESET)
				if stage == "completed stream cancel" {
					first = recoveryExchange(history, nil, nil, &llm.TextDeltaChunk{Text: "done"},
						&llm.DoneChunk{FinishReason: "stop"})
				}
				script := agenttest.NewScriptedProvider(first)
				p := &recoveryPermitProvider{Provider: script}
				var order []string
				p.exited = func() {
					order = append(order, "cleanup")
					if stage == "before retry" || stage == "completed stream cancel" {
						cancel()
					}
				}
				startSleep := make(chan struct{})
				if stage == "sleep cancel" {
					go func() {
						<-startSleep
						// The consumer has reached SleepFor and is blocked on its timer.
						synctest.Wait()
						cancel()
					}()
				}
				a := New(p, tool.NewRegistry(), WithLogger(discardLogger()),
					WithRetryConfig(AgentRetryConfig{MaxRetries: 1, BaseDelay: time.Hour, OnRetry: func(RetryInfo) {
						order = append(order, "callback")
						if stage == "callback cancel" {
							cancel()
						}
						if stage == "sleep cancel" {
							close(startSleep)
						}
					}}))
				stream, err := a.RunStream(ctx, "input")
				if err != nil {
					t.Fatal(err)
				}
				var got error
				start := time.Now()
				for event, err := range stream {
					if _, ok := event.(DoneEvent); ok {
						t.Fatal("cancelled stream completed")
					}
					if _, ok := event.(RetryEvent); ok {
						order = append(order, "event")
						if stage == "event break" {
							break
						}
					}
					if err != nil {
						got = err
					}
				}
				want := []string{"cleanup"}
				switch stage {
				case "event break":
					want = append(want, "event")
					if got != nil {
						t.Fatalf("break error = %v", got)
					}
				case "callback cancel", "sleep cancel":
					want = append(want, "event", "callback")
				}
				if stage != "event break" && !errors.Is(got, context.Canceled) {
					t.Fatalf("cancel error = %v", got)
				}
				if !reflect.DeepEqual(order, want) || time.Since(start) != 0 {
					t.Fatalf("order=%v want=%v elapsed=%v", order, want, time.Since(start))
				}
				if err := script.Verify(); err != nil {
					t.Fatal(err)
				}
			})
		})
	}
}

func TestZeroDeliveryRetryLocalPreparationErrorStaysLocal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		history := []llm.Message{llm.UserMessage("input")}
		p := agenttest.NewScriptedProvider(recoveryExchange(history, nil, syscall.ECONNRESET))
		callbacks := 0
		a := New(p, tool.NewRegistry(), WithLogger(discardLogger()),
			WithRoundContextProvider(func(_ context.Context, request RoundContextRequest) (string, error) {
				if request.Attempt == 2 {
					return "", syscall.EPIPE
				}
				return "", nil
			}),
			WithRetryConfig(AgentRetryConfig{MaxRetries: 2, OnRetry: func(RetryInfo) { callbacks++ }}))
		_, err := a.Run(t.Context(), "input")
		var local *RoundContextError
		if !errors.As(err, &local) || !errors.Is(err, syscall.EPIPE) || local.Attempt != 2 || callbacks != 1 {
			t.Fatalf("local failure=%v callbacks=%d", err, callbacks)
		}
		if err := p.Verify(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestFallbackAndInBandFailureShareRetryBudget(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "exhausted"}[terminal], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				history := []llm.Message{llm.UserMessage("input")}
				last := recoveryExchange(history, nil, nil, &llm.TextDeltaChunk{Text: "done"})
				if terminal {
					last = recoveryExchange(history, nil, syscall.ECONNRESET)
				}
				p := agenttest.NewScriptedProvider(
					recoveryExchange(history, llm.ErrStreamingNotSupported, nil),
					agenttest.Exchange{Method: agenttest.MethodChat, Request: last.Request, ChatErr: syscall.ECONNRESET},
					recoveryExchange(history, nil, syscall.ECONNRESET), last)
				var attempts []int
				var callbacks []RetryInfo
				a := New(p, tool.NewRegistry(), WithLogger(discardLogger()),
					WithRoundContextProvider(func(_ context.Context, req RoundContextRequest) (string, error) {
						attempts = append(attempts, req.Attempt)
						return "", nil
					}),
					WithRetryConfig(AgentRetryConfig{MaxRetries: 2, BaseDelay: time.Second, OnRetry: func(info RetryInfo) {
						callbacks = append(callbacks, info)
					}}))
				stream, err := a.RunStream(t.Context(), "input")
				if err != nil {
					t.Fatal(err)
				}
				var events []RetryInfo
				var got error
				done := false
				for event, err := range stream {
					switch e := event.(type) {
					case RetryEvent:
						events = append(events, e.RetryInfo)
					case DoneEvent:
						done = true
					}
					if err != nil {
						got = err
					}
				}
				if (terminal && !errors.Is(got, syscall.ECONNRESET)) || (!terminal && got != nil) || done == terminal {
					t.Fatalf("terminal=%v error=%v done=%v", terminal, got, done)
				}
				if !reflect.DeepEqual(attempts, []int{1, 2, 3, 4}) || len(events) != 2 || !reflect.DeepEqual(events, callbacks) {
					t.Fatalf("requests=%v retries=%+v callbacks=%+v", attempts, events, callbacks)
				}
				for i, info := range events {
					if info.Attempt != i+2 || info.MaxAttempts != 3 {
						t.Fatalf("retry = %+v", info)
					}
				}
				if err := p.Verify(); err != nil {
					t.Fatal(err)
				}
			})
		})
	}
}
