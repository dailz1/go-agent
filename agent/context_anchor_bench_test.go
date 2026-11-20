package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

func benchmarkAnchoredMultiRound(b *testing.B, snapshot string) {
	ctx := context.Background()
	prefix := benchCompactionHistory(b, 50, 4096, 0)
	provider := NewMockProvider()
	a := New(provider, tool.NewRegistry(), WithLogger(discardLogger()),
		WithRoundContextProvider(func(context.Context, RoundContextRequest) (string, error) {
			return snapshot, nil
		}))
	measurement := requestMeasurement(provider, nil, nil)
	var rounds [32][]llm.Message
	for i := range rounds {
		id := fmt.Sprintf("suffix-%d", i)
		rounds[i] = []llm.Message{
			llm.AssistantToolCallMessage(llm.ToolUseBlock{ID: id, Name: "read", Input: []byte(`{}`)}),
			llm.ToolResultMessage(id, tool.NewTextResult(strings.Repeat("result ", 64))),
			llm.UserMessage("continue"),
		}
	}
	b.ResetTimer()
	for b.Loop() {
		history := append(make([]llm.Message, 0, len(prefix)+3*len(rounds)), prefix...)
		state := &contextRuntime{}
		state.activateMeasurement(measurement, &llm.Usage{InputTokens: 2000, OutputTokens: 100}, history, nil)
		for i, messages := range rounds {
			history = append(history, messages...)
			estimated := state.estimate(history)
			if state.anchor == nil || estimated <= 2100 {
				b.Fatal("anchored suffix was not counted")
			}
			if snapshot != "" {
				fitter := state.overlayFitter(history)
				if _, err := a.roundContextOutbound(ctx, history, RoundContextRequest{Round: i, Attempt: 1},
					128*1024, estimated, fitter); err != nil {
					b.Fatal(err)
				}
			}
		}
	}
}

func BenchmarkAnchoredMultiRound(b *testing.B) {
	b.Run("suffix", func(b *testing.B) { benchmarkAnchoredMultiRound(b, "") })
	b.Run("large_overlay", func(b *testing.B) { benchmarkAnchoredMultiRound(b, strings.Repeat("snapshot ", 4096)) })
}
