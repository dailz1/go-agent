package agent

// Pins the issue #22 contract: user-supplied llm.Option functions are
// resolved exactly once per round by freezeLLMOptions at round top, and the
// frozen snapshot — not the original functions — is what every downstream
// consumer sees: the window lookup, the budget derivation, and every direct
// provider invocation including retry attempts. A dynamic option whose
// resolved values change on every evaluation must therefore yield one
// consistent view per round; if any kernel path ever re-evaluates the
// original functions after freezing, the evaluation counter exceeds the
// round count and the diverging "later" values leak into the request.

import (
	"context"
	"fmt"
	"iter"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

// optionsRecordingProvider records the resolved llm.Options each provider
// invocation received.
type optionsRecordingProvider struct {
	inner llm.Provider
	seen  []llm.Options
}

func (p *optionsRecordingProvider) Name() string { return "options_recording" }

func (p *optionsRecordingProvider) ChatStream(ctx context.Context, messages []llm.Message, tools []tool.ToolInfo, opts ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	p.seen = append(p.seen, llm.ApplyOptions(opts))
	return p.inner.ChatStream(ctx, messages, tools, opts...)
}

func (p *optionsRecordingProvider) Chat(ctx context.Context, messages []llm.Message, tools []tool.ToolInfo, opts ...llm.Option) (*llm.Message, *llm.Usage, error) {
	p.seen = append(p.seen, llm.ApplyOptions(opts))
	return p.inner.Chat(ctx, messages, tools, opts...)
}

// newDivergingOption returns an option whose resolved values change on every
// evaluation (model-N, temperature 0.1·N): a second evaluation shows up in
// both the counter and the request.
func newDivergingOption() (llm.Option, *int) {
	calls := 0
	return func(o *llm.Options) {
		calls++
		o.Model = fmt.Sprintf("model-%d", calls)
		temp := 0.1 * float64(calls)
		o.Temperature = &temp
	}, &calls
}

func TestLLMOptionsFrozenOncePerRoundAcrossRetries(t *testing.T) {
	t.Parallel()

	p := &scriptedProvider{steps: []scriptedStep{
		// Attempt 1 fails pre-stream with a retryable error; the retry must
		// reuse the frozen snapshot.
		{stream: true, err: &llm.APIError{StatusCode: 503}},
		{stream: true, chunks: probeTextChunks()},
	}}
	rec := &optionsRecordingProvider{inner: p}
	opt, calls := newDivergingOption()
	ag := newIsolationAgent(rec, 1, WithLLMOptions(opt))

	if _, err := ag.RunWithHistory(context.Background(), nil, "go"); err != nil {
		t.Fatalf("RunWithHistory() error = %v", err)
	}

	if got := *calls; got != 1 {
		t.Errorf("option evaluations = %d, want 1 (frozen once per round; retries must not re-resolve)", got)
	}
	if len(rec.seen) != 2 {
		t.Fatalf("provider invocations = %d, want 2 (initial attempt + retry)", len(rec.seen))
	}
	for i, o := range rec.seen {
		if o.Model != "model-1" {
			t.Errorf("attempt %d: provider saw model %q, want frozen %q", i+1, o.Model, "model-1")
		}
		if o.Temperature == nil || *o.Temperature != 0.1 {
			t.Errorf("attempt %d: provider saw temperature %v, want frozen 0.1", i+1, o.Temperature)
		}
	}
}

func TestLLMOptionsResolvedPerRound(t *testing.T) {
	t.Parallel()

	p := &scriptedProvider{steps: []scriptedStep{
		// Round-local freeze: round 2 must see the second evaluation, not a
		// run-global snapshot.
		{stream: true, chunks: probeToolCallChunks()},
		{stream: true, chunks: probeTextChunks()},
	}}
	rec := &optionsRecordingProvider{inner: p}
	opt, calls := newDivergingOption()
	ag := newIsolationAgent(rec, 0, WithLLMOptions(opt))

	if _, err := ag.RunWithHistory(context.Background(), nil, "go"); err != nil {
		t.Fatalf("RunWithHistory() error = %v", err)
	}

	if got := *calls; got != 2 {
		t.Errorf("option evaluations = %d, want 2 (exactly one per round)", got)
	}
	if len(rec.seen) != 2 {
		t.Fatalf("provider invocations = %d, want 2 (one per round)", len(rec.seen))
	}
	for i, wantModel := range []string{"model-1", "model-2"} {
		if got := rec.seen[i].Model; got != wantModel {
			t.Errorf("round %d: provider saw model %q, want %q", i+1, got, wantModel)
		}
	}
}
