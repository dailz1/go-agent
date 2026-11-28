package agent

import (
	"context"
	"fmt"
	"iter"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

type runOptionRequest struct {
	prompt string
	model  string
	tokens int
}

type runOptionConcurrentProvider struct {
	mu       sync.Mutex
	requests map[string][]runOptionRequest
	started  chan struct{}
	release  chan struct{}
}

func (*runOptionConcurrentProvider) Name() string { return "run-option-concurrent" }
func (*runOptionConcurrentProvider) Chat(context.Context, []llm.Message, []tool.ToolInfo, ...llm.Option) (*llm.Message, *llm.Usage, error) {
	return nil, nil, llm.ErrStreamingNotSupported
}
func (p *runOptionConcurrentProvider) ChatStream(ctx context.Context, messages []llm.Message, _ []tool.ToolInfo, opts ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	key := messageString(messages[1])
	options := llm.ApplyOptions(opts)
	p.mu.Lock()
	n := len(p.requests[key])
	p.requests[key] = append(p.requests[key], runOptionRequest{
		prompt: messageString(messages[0]), model: options.Model, tokens: options.MaxTokens,
	})
	p.mu.Unlock()
	if n == 0 && key != "default" {
		select {
		case p.started <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		select {
		case <-p.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return func(yield func(llm.Chunk, error) bool) {
		for _, chunk := range toolCallChunks(fmt.Sprintf("%s-%d", key, n), "echo") {
			if !yield(chunk, nil) {
				return
			}
		}
	}, nil
}

func TestRunOptionsConcurrentIsolation(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	p := &runOptionConcurrentProvider{
		requests: make(map[string][]runOptionRequest), started: make(chan struct{}, 2), release: make(chan struct{}),
	}
	a := New(p, tool.NewRegistry(), WithMaxIter(2), WithSystemPrompt("base"),
		WithLLMOptions(llm.WithModel("base"), llm.WithMaxTokens(20)), WithLogger(discardLogger()))
	shared := RunWithMaxIter(3)
	type outcome struct {
		key    string
		result *RunResult
		err    error
	}
	finished := make(chan outcome, 2)
	for _, tc := range []struct {
		key    string
		budget int
		tokens int
	}{
		{key: "one", budget: 1, tokens: 11},
		{key: "three", budget: 3, tokens: 33},
	} {
		go func() {
			opts := []RunOption{shared, RunWithSystemPrompt(tc.key),
				RunWithLLMOptions(llm.WithModel(tc.key), llm.WithMaxTokens(tc.tokens))}
			if tc.budget == 1 {
				opts = append(opts, RunWithMaxIter(1))
			}
			result, err := a.Run(ctx, tc.key, opts...)
			finished <- outcome{key: tc.key, result: result, err: err}
		}()
	}
	for range 2 {
		select {
		case <-p.started:
		case <-ctx.Done():
			t.Fatal("calls did not overlap")
		}
	}
	close(p.release)
	for range 2 {
		select {
		case got := <-finished:
			want := 3
			if got.key == "one" {
				want = 1
			}
			if got.err != nil || got.result == nil || !got.result.Truncated || got.result.ToolCalls != want {
				t.Fatalf("outcome=%+v, want %d skipped-boundary calls", got, want)
			}
		case <-ctx.Done():
			t.Fatal("concurrent run did not finish")
		}
	}
	result, err := a.Run(ctx, "default")
	if err != nil || !result.Truncated || result.ToolCalls != 2 {
		t.Fatalf("default result=%+v err=%v", result, err)
	}
	want := map[string][]runOptionRequest{
		"one":     {{prompt: "one", model: "one", tokens: 11}},
		"three":   {{prompt: "three", model: "three", tokens: 33}, {prompt: "three", model: "three", tokens: 33}, {prompt: "three", model: "three", tokens: 33}},
		"default": {{prompt: "base", model: "base", tokens: 20}, {prompt: "base", model: "base", tokens: 20}},
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !reflect.DeepEqual(p.requests, want) {
		t.Fatalf("requests=%+v, want %+v", p.requests, want)
	}
}
