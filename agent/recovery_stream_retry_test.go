package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"reflect"
	"syscall"
	"testing"
	"testing/synctest"

	"github.com/dailz1/go-agent/agenttest"
	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

func TestZeroDeliveryRetryPreservesRoundRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		reg := tool.NewRegistry()
		reg.MustRegister(&gatedTool{info: tool.ToolInfo{Name: "probe", Description: "probe tool"}})
		history := []llm.Message{llm.SystemMessage("policy"), llm.UserMessage("input")}
		outbound := append(deepCopyMessages(history), roundContextMessage("snapshot"))
		opts := []llm.Option{llm.WithModel("frozen")}
		fail := recoveryExchange(outbound, nil, fmt.Errorf("read: %w", syscall.ECONNRESET))
		good := recoveryExchange(outbound, nil, nil, &llm.TextDeltaChunk{Text: "done"}, &llm.DoneChunk{FinishReason: "stop"})
		for _, e := range []*agenttest.Exchange{&fail, &good} {
			e.Request.Tools, e.Request.Options = reg.List(), llm.ApplyOptions(opts)
		}
		script := agenttest.NewScriptedProvider(fail, good)
		p := &recoveryPermitProvider{Provider: script}
		var order []string
		p.exited = func() { order = append(order, "cleanup") }
		var requests []RoundContextRequest
		var callbacks []RetryInfo
		a := New(p, reg, WithLogger(discardLogger()), WithSystemPrompt("policy"), WithLLMOptions(opts...),
			WithRoundContextProvider(func(_ context.Context, req RoundContextRequest) (string, error) {
				requests = append(requests, req)
				order = append(order, "request")
				return "snapshot", nil
			}),
			WithRetryConfig(AgentRetryConfig{MaxRetries: 1, OnRetry: func(info RetryInfo) {
				if p.held {
					t.Fatal("callback ran before failed iterator released")
				}
				order = append(order, "callback")
				callbacks = append(callbacks, info)
			}}))
		stream, err := a.RunStream(t.Context(), "input")
		if err != nil {
			t.Fatal(err)
		}
		var retries []RetryInfo
		var done DoneEvent
		for event, err := range stream {
			if err != nil {
				t.Fatal(err)
			}
			switch e := event.(type) {
			case RetryEvent:
				if p.held {
					t.Fatal("retry event preceded iterator cleanup")
				}
				order = append(order, "event")
				retries = append(retries, e.RetryInfo)
			case DoneEvent:
				done = e
			}
		}
		wantRequests := []RoundContextRequest{{Round: 0, Attempt: 1}, {Round: 0, Attempt: 2}}
		if !reflect.DeepEqual(requests, wantRequests) ||
			!reflect.DeepEqual(order, []string{"request", "cleanup", "event", "callback", "request", "cleanup"}) {
			t.Fatalf("requests=%+v order=%v", requests, order)
		}
		if len(retries) != 1 || !reflect.DeepEqual(retries, callbacks) || !reflect.DeepEqual(done.Retries, retries) {
			t.Fatalf("retry event=%+v callback=%+v done=%+v", retries, callbacks, done.Retries)
		}
		if retries[0].Attempt != 2 || retries[0].MaxAttempts != 2 ||
			!errors.Is(retries[0].Err, syscall.ECONNRESET) || messageString(done.Message) != "done" {
			t.Fatalf("retry=%+v done=%+v", retries, done)
		}
		if !reflect.DeepEqual(done.History, append(history, llm.AssistantMessage("done"))) {
			t.Fatalf("transient overlay or failed attempt entered history: %+v", done.History)
		}
		if err := script.Verify(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestDeliveredNetworkFailureNeverRetries(t *testing.T) {
	for _, tc := range []struct {
		name   string
		chunk  llm.Chunk
		paired bool
	}{
		{"text", &llm.TextDeltaChunk{Text: "partial"}, false},
		{"reasoning", &llm.ReasoningDeltaChunk{Text: "partial"}, false},
		{"tool start", &llm.ToolCallStartChunk{ID: "unexecuted", Name: "missing"}, false},
		{"tool args", &llm.ToolCallArgsChunk{Delta: "{}"}, false},
		{"reasoning item", &llm.ReasoningItemChunk{}, false},
		{"usage only", &llm.DoneChunk{Usage: &llm.Usage{InputTokens: 9}}, false},
		{"typed nil", (*llm.TextDeltaChunk)(nil), false},
		{"same return", &llm.TextDeltaChunk{Text: "paired"}, true},
		{"typed nil same return", (*llm.TextDeltaChunk)(nil), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			history := []llm.Message{llm.UserMessage("input")}
			first := recoveryExchange(history, nil, syscall.ECONNRESET, tc.chunk)
			if tc.paired {
				first.StreamChunks = nil
			}
			p := agenttest.NewScriptedProvider(first)
			var provider llm.Provider = p
			if tc.paired {
				provider = recoveryStreamWrapper{Provider: p, wrap: func(stream iter.Seq2[llm.Chunk, error]) iter.Seq2[llm.Chunk, error] {
					return func(yield func(llm.Chunk, error) bool) {
						for _, err := range stream {
							if !yield(tc.chunk, err) {
								return
							}
						}
					}
				}}
			}
			callbacks, retries := 0, 0
			a := New(provider, tool.NewRegistry(), WithLogger(discardLogger()),
				WithRetryConfig(AgentRetryConfig{OnRetry: func(RetryInfo) { callbacks++ }}))
			stream, err := a.RunStream(t.Context(), "input")
			if err != nil {
				t.Fatal(err)
			}
			var got error
			for event, err := range stream {
				if _, ok := event.(RetryEvent); ok {
					retries++
				}
				if err != nil {
					got = err
				}
				if _, ok := event.(DoneEvent); ok {
					t.Fatal("partial response completed")
				}
			}
			if !errors.Is(got, syscall.ECONNRESET) || retries != 0 || callbacks != 0 {
				t.Fatalf("error=%v events=%d callbacks=%d", got, retries, callbacks)
			}
			if err := p.Verify(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestTransportRetryBudgetSharedAcrossFailurePositions(t *testing.T) {
	for _, tc := range []struct {
		name       string
		maxRetries int
		outer      []bool
	}{
		{"setup then in-band", 2, []bool{true, false, false}},
		{"in-band then setup", 2, []bool{false, true, true}},
		{"disabled", -1, []bool{false}},
		{"default", 0, []bool{false, true, false, false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				history := []llm.Message{llm.UserMessage("input")}
				var exchanges []agenttest.Exchange
				for _, outer := range tc.outer {
					e := recoveryExchange(history, nil, io.ErrUnexpectedEOF)
					if outer {
						e.StreamErr, e.StreamOuterErr = nil, io.ErrUnexpectedEOF
					}
					exchanges = append(exchanges, e)
				}
				p := agenttest.NewScriptedProvider(exchanges...)
				var callbacks, events []RetryInfo
				a := New(p, tool.NewRegistry(), WithLogger(discardLogger()),
					WithRetryConfig(AgentRetryConfig{MaxRetries: tc.maxRetries, OnRetry: func(info RetryInfo) {
						callbacks = append(callbacks, info)
					}}))
				stream, err := a.RunStream(t.Context(), "input")
				if err != nil {
					t.Fatal(err)
				}
				var got error
				for event, err := range stream {
					if e, ok := event.(RetryEvent); ok {
						events = append(events, e.RetryInfo)
					}
					if err != nil {
						got = err
					}
				}
				if !errors.Is(got, io.ErrUnexpectedEOF) || len(events) != len(tc.outer)-1 ||
					!reflect.DeepEqual(events, callbacks) {
					t.Fatalf("error=%v events=%+v callbacks=%+v", got, events, callbacks)
				}
				for i, info := range events {
					if info.Attempt != i+2 || info.MaxAttempts != len(tc.outer) || !errors.Is(info.Err, io.ErrUnexpectedEOF) {
						t.Fatalf("retry %d = %+v", i, info)
					}
				}
				if err := p.Verify(); err != nil {
					t.Fatal(err)
				}
			})
		})
	}
}
