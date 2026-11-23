package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"iter"
	"math/rand"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

func TestContextOverlayFitterCacheParity(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(19))
	alphabet := []rune("ab中😀\n\t\\\"<>")
	for i := 0; i < 120; i++ {
		t.Run(fmt.Sprintf("case_%03d", i), func(t *testing.T) {
			text := make([]rune, rng.Intn(500))
			for j := range text {
				text[j] = alphabet[rng.Intn(len(alphabet))]
			}
			var history []llm.Message
			if i%3 != 0 {
				history = []llm.Message{llm.UserMessage(string(text[:len(text)/2]))}
			} else if i%2 == 0 {
				history = []llm.Message{}
			}
			if i%7 == 0 {
				history = append(history, llm.AssistantToolCallMessage(&llm.ToolUseBlock{ID: "bad", Name: "bad", Input: []byte("{")}))
			}
			w := 500 + rng.Intn(1500)
			a := New(NewMockProvider(), tool.NewRegistry(), WithLogger(discardLogger()))
			fitter := newOverlayFitter(history)
			uncached := overlayFitter{history: history, baseRunes: estimateRunes(history)}
			got, truncated, err := a.boundedRoundContextMessage(fitter, string(text), w, uint64(fitter.baseRunes))
			want, wantTruncated, wantErr := a.boundedRoundContextMessage(uncached, string(text), w, uint64(uncached.baseRunes))
			if !reflect.DeepEqual(got, want) || truncated != wantTruncated ||
				errors.Is(err, ErrCompactionBudgetExceeded) != errors.Is(wantErr, ErrCompactionBudgetExceeded) {
				t.Fatalf("cached = %#v/%v/%v; uncached = %#v/%v/%v", got, truncated, err, want, wantTruncated, wantErr)
			}
		})
	}
}

func TestContextRuntimeOverlayUsesMeasuredCanonical(t *testing.T) {
	t.Parallel()
	for _, usage := range []int64{100, 1900} {
		t.Run(fmt.Sprint(usage), func(t *testing.T) {
			p := NewMockStreamingProvider([][]llm.Chunk{
				contextChunks(strings.Repeat("x", 2400), &llm.Usage{InputTokens: usage}), textChunks("done"),
			})
			a := New(p, tool.NewRegistry(), WithContextWindowTokens(2000), WithMaxIter(2),
				WithRoundContextProvider(func(context.Context, RoundContextRequest) (string, error) { return "snapshot", nil }),
				WithFinishRound(contextContinue), WithLogger(discardLogger()))
			_, err := a.Run(t.Context(), "q")
			if usage == 100 {
				if err != nil || len(p.AllMessages) != 2 || estimateRunes(p.AllMessages[1]) <= 2000 {
					t.Fatalf("measured-fit overlay request: calls %d, error %v", len(p.AllMessages), err)
				}
			} else if !errors.Is(err, ErrCompactionBudgetExceeded) || len(p.AllMessages) != 1 {
				t.Fatalf("canonical token cap: calls %d, error %v", len(p.AllMessages), err)
			}
		})
	}
}

func TestContextRuntimeSummarizerUsageNeverAnchors(t *testing.T) {
	t.Parallel()
	p := &contextSummaryProvider{
		MockStreamingProvider: *NewMockStreamingProvider([][]llm.Chunk{
			contextChunks("measured", &llm.Usage{InputTokens: 9900, OutputTokens: 100}),
			textChunks("suffix"), textChunks("done"),
		}),
	}
	var buf bytes.Buffer
	a := New(p, tool.NewRegistry(), WithContextWindowTokens(7500), WithMaxIter(3),
		WithCompactor(NewSummarizationCompactor()), WithFinishRound(contextContinue),
		WithLogger(contextTestLogger(&buf)))
	history := []llm.Message{llm.UserMessage("first"), llm.AssistantMessage(strings.Repeat("x", 3500)), llm.UserMessage("latest")}
	if _, err := a.RunWithHistory(t.Context(), history, "q"); err != nil {
		t.Fatal(err)
	}
	records := contextBudgetRecords(t, &buf)
	if p.summaries != 1 || len(records) != 3 || records[2].Valid ||
		records[2].Estimate != estimateRunes(p.AllMessages[2]) {
		t.Fatalf("summaries = %d, budget decisions = %+v", p.summaries, records)
	}
}

type contextSummaryProvider struct {
	MockStreamingProvider
	summaries int
}

func (p *contextSummaryProvider) Chat(context.Context, []llm.Message, []tool.ToolInfo, ...llm.Option) (*llm.Message, *llm.Usage, error) {
	p.summaries++
	message := llm.AssistantMessage("summary")
	return &message, &llm.Usage{InputTokens: 1, OutputTokens: 1}, nil
}

func TestContextRuntimeConcurrentRunsAreIndependent(t *testing.T) {
	t.Parallel()
	var ready sync.WaitGroup
	ready.Add(2)
	p := &contextConcurrentProvider{ready: &ready}
	var mu sync.Mutex
	compactions := map[string]int{}
	compactor := CompactFunc(func(_ context.Context, history []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
		mu.Lock()
		compactions[messageText(&history[0])]++
		mu.Unlock()
		history[1] = llm.AssistantMessage("kept")
		return CompactionResult{History: history, Changed: true}, nil
	})
	a := New(p, tool.NewRegistry(), WithContextWindowTokens(7500), WithMaxIter(2),
		WithCompactor(compactor), WithFinishRound(contextContinue), WithLogger(discardLogger()))
	errs := make(chan error, 2)
	for _, input := range []string{"low", "high"} {
		go func() {
			_, err := a.Run(t.Context(), input)
			errs <- err
		}()
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(compactions, map[string]int{"high": 1}) {
		t.Fatalf("per-run compactions = %v", compactions)
	}
}

type contextConcurrentProvider struct {
	MockProvider
	ready *sync.WaitGroup
}

func (p *contextConcurrentProvider) ChatStream(_ context.Context, history []llm.Message, _ []tool.ToolInfo, _ ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	chunks := textChunks("done")
	if len(history) == 1 {
		p.ready.Done()
		p.ready.Wait()
		input := int64(100)
		if messageText(&history[0]) == "high" {
			input = 10000
		}
		chunks = contextChunks(strings.Repeat("x", 7000), &llm.Usage{InputTokens: input})
	}
	return func(yield func(llm.Chunk, error) bool) {
		for _, chunk := range chunks {
			if !yield(chunk, nil) {
				return
			}
		}
	}, nil
}
