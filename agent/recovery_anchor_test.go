package agent

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/agenttest"
	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

func TestRecoveryStrictDecreaseUsesRuneBaselineWithAnchor(t *testing.T) {
	for _, tc := range []struct {
		name   string
		text   string
		reason string
	}{
		{"reduced", strings.Repeat("s", 600), ""},
		{"same_runes", strings.Repeat("s", 4000), "no_progress"},
		{"larger", strings.Repeat("s", 4500), "no_progress"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := []llm.Message{llm.UserMessage("q")}
			reply := strings.Repeat("x", 4000)
			rejected := append(deepCopyMessages(first), llm.AssistantMessage(reply))
			replacement := deepCopyMessages(rejected)
			replacement[1] = llm.AssistantMessage(tc.text)
			before, after := estimateRunes(rejected), estimateRunes(replacement)
			exchanges := []agenttest.Exchange{
				recoveryExchange(first, nil, nil, contextChunks(reply, &llm.Usage{InputTokens: 100})...),
				recoveryExchange(rejected, overflowTestError(), nil),
			}
			if tc.reason == "" {
				exchanges = append(exchanges, recoveryExchange(replacement, nil, nil, textChunks("done")...))
			}
			for i := range exchanges {
				exchanges[i].Request.Options = llm.ApplyOptions([]llm.Option{llm.WithModel("x")})
			}
			provider := agenttest.NewScriptedProvider(exchanges...)
			st := store.NewMemory()
			var buf bytes.Buffer
			compactions := 0
			a := New(provider, tool.NewRegistry(), WithStore(st), WithLLMOptions(llm.WithModel("x")),
				WithContextWindowTokens(2000), WithMaxIter(3), WithLogger(contextTestLogger(&buf)),
				WithFinishRound(func(_ context.Context, f FinishRoundContext) (FinishRoundDecision, error) {
					if f.Round == 0 {
						return FinishRoundDecision{Action: FinishRoundContinue}, nil
					}
					return FinishRoundDecision{}, nil
				}),
				WithCompactor(CompactFunc(func(_ context.Context, h []llm.Message, budget CompactionBudget,
					_ llm.Provider, _ []llm.Option) (CompactionResult, error) {
					compactions++
					if !reflect.DeepEqual(h, rejected) || budget.MaxRunes != 1200 {
						t.Fatalf("recovery input/budget: %d runes, budget %d; want %d runes, budget 1200",
							estimateRunes(h), budget.MaxRunes, before)
					}
					return CompactionResult{History: replacement, Changed: true}, nil
				})))

			result, err := a.Run(t.Context(), "q", WithThread("recovery"))
			if tc.reason != "" {
				requireRecoveryError(t, err, tc.reason, 1)
			} else if err != nil {
				t.Fatal(err)
			} else if after >= before || !reflect.DeepEqual(result.History[:len(replacement)], replacement) {
				t.Fatalf("recovery did not adopt the rune reduction: before %d, after %d", before, after)
			}
			if compactions != 1 {
				t.Fatalf("compactions = %d, want 1 after overflow", compactions)
			}
			decisions := contextBudgetRecords(t, &buf)
			if len(decisions) != 2 || !decisions[1].Valid || decisions[1].Estimate != 100 || before <= 2000 {
				t.Fatalf("fixture did not reach overflow with a fitting anchor: %+v; runes %d", decisions, before)
			}
			var phases []string
			for _, record := range logRecords(t, st, "recovery") {
				if record.Kind != store.KindContextRecovery {
					continue
				}
				p, err := decodeContextRecovery(record)
				if err != nil {
					t.Fatal(err)
				}
				phases = append(phases, p.Phase)
				if p.BeforeRunes != before || p.RejectedOutboundRunes != before {
					t.Fatalf("%s proof uses non-rune baseline: before %d, rejected %d; want %d",
						p.Phase, p.BeforeRunes, p.RejectedOutboundRunes, before)
				}
			}
			wantPhases := []string{"pending", "preparing", "dispatched"}
			if tc.reason != "" {
				wantPhases[2] = "blocked"
			}
			if !reflect.DeepEqual(phases, wantPhases) {
				t.Fatalf("recovery phases = %v, want %v", phases, wantPhases)
			}
			if err := provider.Verify(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
