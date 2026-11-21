package modelmeta

import (
	"context"
	"io"
	"iter"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dailz1/go-agent/agent"
	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/llm/codex"
	"github.com/dailz1/go-agent/llm/glm"
	"github.com/dailz1/go-agent/llm/openai"
	"github.com/dailz1/go-agent/llm/openairesponses"
	"github.com/dailz1/go-agent/tool"
)

func TestFourAdaptersDelayExpiryAndExactNames(t *testing.T) {
	c := testCatalog()
	for _, tc := range []struct {
		name, source string
		make         func(map[string]int) llm.Provider
		want         int
	}{
		{"glm", "zhipuai", func(w map[string]int) llm.Provider {
			return glm.NewProvider("key", "m", glm.WithModelContextWindows(w))
		}, 1000},
		{"openai", "openai", func(w map[string]int) llm.Provider {
			return openai.NewProvider("key", "m", openai.WithModelContextWindows(w))
		}, 1000},
		{"responses", "openai", func(w map[string]int) llm.Provider {
			return openairesponses.NewProvider("key", "m", openairesponses.WithModelContextWindows(w))
		}, 1000},
		{"codex", "openai-codex", func(w map[string]int) llm.Provider {
			return codex.NewProvider("m", codex.WithModelContextWindows(w))
		}, 2000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at := fixedAt
			setClock(t, func() time.Time { return at })
			windows, err := c.Windows(tc.source, at, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			saved, err := c.Windows(tc.source, at, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			base := tc.make(windows)
			windows["m"] = -1
			p, err := c.Guard(base, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			for _, model := range []string{"", "m", "M", "m-2026-11-20", "unknown"} {
				n, ok := p.(llm.ContextWindowProvider).ContextWindow(model)
				wantOK := model == "" || model == "m"
				if ok != wantOK || (ok && n != tc.want) || (!ok && n != 0) {
					t.Fatalf("%q = %d, %v", model, n, ok)
				}
			}
			for _, age := range []time.Duration{time.Hour, time.Hour + 1} {
				at = fixedAt.Add(age)
				if n, ok := p.(llm.ContextWindowProvider).ContextWindow(""); ok || n != 0 {
					t.Fatalf("existing provider expiry = %d, %v", n, ok)
				}
			}
			// Export before expiry, construct afterwards: the naked map and
			// adapter stay static, while a newly constructed Guard still expires.
			lateBase := tc.make(saved)
			late, err := c.Guard(lateBase, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			if n, ok := lateBase.(llm.ContextWindowProvider).ContextWindow(""); !ok || n != tc.want {
				t.Fatalf("bare provider lost static value: %d, %v", n, ok)
			}
			if n, ok := late.(llm.ContextWindowProvider).ContextWindow(""); ok || n != 0 {
				t.Fatalf("late guard declared stale value: %d, %v", n, ok)
			}
			stale, err := c.Windows(tc.source, at, time.Hour)
			if err == nil || stale != nil {
				t.Fatalf("stale export = %v, %v", stale, err)
			}
			if n, ok := tc.make(stale).(llm.ContextWindowProvider).ContextWindow(""); ok || n != 0 {
				t.Fatalf("nil-map provider = %d, %v", n, ok)
			}
		})
	}
}

func TestAgentKeepsStartedRoundBudgetAndFallsBackNextRound(t *testing.T) {
	at := fixedAt
	setClock(t, func() time.Time { return at })
	base := &stubProvider{
		stream: func(context.Context, []llm.Message, []tool.ToolInfo, ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
			return func(yield func(llm.Chunk, error) bool) {
				if !yield(llm.TextDeltaChunk{Text: strings.Repeat("a", 12000)}, nil) {
					return
				}
				yield(llm.DoneChunk{FinishReason: "stop"}, nil)
			}, nil
		},
	}
	p, err := testCatalog().Guard(base, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	budgets := []int{}
	compactor := agent.CompactFunc(func(_ context.Context, h []llm.Message, b agent.CompactionBudget, _ llm.Provider, _ []llm.Option) (agent.CompactionResult, error) {
		budgets = append(budgets, b.MaxRunes)
		at = fixedAt.Add(time.Hour)
		for i := range h {
			if h[i].Role == llm.RoleUser {
				h[i] = llm.UserMessage("kept user")
			}
			if h[i].Role == llm.RoleAssistant {
				h[i] = llm.AssistantMessage("kept assistant")
			}
		}
		return agent.CompactionResult{History: h, Changed: true}, nil
	})
	a := agent.New(p, tool.NewRegistry(),
		agent.WithContextWindowTokens(10000),
		agent.WithCompactor(compactor),
		agent.WithMaxIter(2),
		agent.WithFinishRound(func(context.Context, agent.FinishRoundContext) (agent.FinishRoundDecision, error) {
			return agent.FinishRoundDecision{Action: agent.FinishRoundContinue}, nil
		}),
		agent.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)
	if _, err := a.Run(t.Context(), strings.Repeat("u", 12000)); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(budgets, []int{800, 8000}) {
		t.Fatalf("actual kernel budgets = %v; want [800 8000]", budgets)
	}
}
