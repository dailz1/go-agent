package agent

import (
	"bytes"
	"context"
	"errors"
	"iter"
	"reflect"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

type runOptionWindowProvider struct {
	*MockStreamingProvider
	models []string
}

func (p *runOptionWindowProvider) ContextWindow(model string) (int, bool) {
	p.models = append(p.models, model)
	if model == "run" {
		return 2000, true
	}
	return 10000, true
}

func TestRunOptionsWindowCompactionAndRecovery(t *testing.T) {
	for _, overflow := range []bool{false, true} {
		name := "proactive"
		if overflow {
			name = "overflow"
		}
		t.Run(name, func(t *testing.T) {
			input := strings.Repeat("x", 1800)
			short := []llm.Message{llm.UserMessage("short")}
			mock := NewMockStreamingProvider([][]llm.Chunk{textChunks("done")})
			p := &runOptionWindowProvider{MockStreamingProvider: mock}
			var provider llm.Provider = p
			if overflow {
				provider = &runOptionOverflowProvider{runOptionWindowProvider: p}
				input = strings.Repeat("x", 1000) // Below the proactive trigger.
			}
			compacted := 0
			compactor := CompactFunc(func(_ context.Context, h []llm.Message, b CompactionBudget, got llm.Provider, opts []llm.Option) (CompactionResult, error) {
				compacted++
				effective := llm.ApplyOptions(opts)
				if got != provider || effective.Model != "run" || effective.MaxTokens != 17 || b.MaxRunes >= 2000 {
					t.Errorf("compaction provider/options/budget=%T/%+v/%d", got, effective, b.MaxRunes)
				}
				return CompactionResult{History: short, Changed: true}, nil
			})
			a := New(provider, tool.NewRegistry(), WithContextWindowTokens(10000),
				WithLLMOptions(llm.WithModel("constructor")), WithCompactor(compactor), WithLogger(discardLogger()))
			result, err := a.Run(t.Context(), input, RunWithLLMOptions(llm.WithModel("run"), llm.WithMaxTokens(17)))
			if err != nil {
				t.Fatal(err)
			}
			effective := llm.ApplyOptions(mock.LastOptions)
			if compacted != 1 || !reflect.DeepEqual(p.models, []string{"run"}) || effective.Model != "run" ||
				effective.MaxTokens != 17 || !reflect.DeepEqual(result.History[:1], short) {
				t.Fatalf("compactions=%d models=%v options=%+v", compacted, p.models, effective)
			}
		})
	}
}

type runOptionOverflowProvider struct {
	*runOptionWindowProvider
	rejected bool
}

func (p *runOptionOverflowProvider) ChatStream(ctx context.Context, messages []llm.Message, tools []tool.ToolInfo, opts ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	effective := llm.ApplyOptions(opts)
	if effective.Model != "run" || effective.MaxTokens != 17 {
		return nil, errors.New("wrong run options")
	}
	if !p.rejected {
		p.rejected = true
		return nil, overflowTestError()
	}
	return p.MockStreamingProvider.ChatStream(ctx, messages, tools, opts...)
}

func TestRunOptionsMeasurementUsesRoundModel(t *testing.T) {
	var buf bytes.Buffer
	p := NewMockStreamingProvider([][]llm.Chunk{
		contextChunks("measured", &llm.Usage{InputTokens: 200}),
		contextChunks("done", &llm.Usage{InputTokens: 300}),
	})
	calls := 0
	runModel := func(o *llm.Options) {
		calls++
		o.Model = []string{"run-a", "run-b"}[calls-1]
	}
	a := New(p, tool.NewRegistry(), WithLogger(contextTestLogger(&buf)), WithMaxIter(2),
		WithLLMOptions(llm.WithModel("constructor")), WithFinishRound(contextContinue))
	if _, err := a.Run(t.Context(), "task", RunWithLLMOptions(runModel)); err != nil {
		t.Fatal(err)
	}
	records := contextBudgetRecords(t, &buf)
	if calls != 2 || len(records) != 2 || records[1].Reason != "model_changed" || records[1].Valid {
		t.Fatalf("option evaluations=%d measurements=%+v", calls, records)
	}
	if model := llm.ApplyOptions(p.LastOptions).Model; model != "run-b" {
		t.Fatalf("provider model=%q", model)
	}
}
