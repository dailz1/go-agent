package agenttool_test

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dailz1/go-agent/agent"
	"github.com/dailz1/go-agent/agenttool"
	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

func TestExecuteSerializesOneAdapter(t *testing.T) {
	provider := newBlockingProvider()
	adapter := agenttool.New(agent.New(provider, tool.NewRegistry()), agenttool.Config{Name: "worker"})
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { _, err := adapter.Execute(t.Context(), []byte(`{"input":"one"}`)); first <- err }()
	await(t, provider.started)
	ctx := &doneObservedContext{Context: t.Context(), observed: make(chan struct{})}
	go func() { _, err := adapter.Execute(ctx, []byte(`{"input":"two"}`)); second <- err }()
	await(t, ctx.observed)
	if provider.calls.Load() != 1 {
		t.Fatalf("child calls while queued = %d, want 1", provider.calls.Load())
	}
	close(provider.release)
	if err := await(t, first); err != nil {
		t.Fatalf("first Execute: %v", err)
	}
	if err := await(t, second); err != nil {
		t.Fatalf("second Execute: %v", err)
	}
	if provider.calls.Load() != 2 || provider.maxActive.Load() != 1 {
		t.Fatalf("child calls/max active = %d/%d, want 2/1", provider.calls.Load(), provider.maxActive.Load())
	}
}

func TestParentToolConcurrencyMatchesAdapterScope(t *testing.T) {
	t.Run("same adapter", func(t *testing.T) {
		provider := newBlockingProvider()
		adapter := agenttool.New(agent.New(provider, tool.NewRegistry()), agenttool.Config{Name: "worker"})
		registry := tool.NewRegistry()
		registry.MustRegister(adapter)
		parent := agent.New(&toolCallsProvider{calls: []llm.ToolUseBlock{
			{ID: "one", Name: "worker", Input: json.RawMessage(`{"input":"one"}`)},
			{ID: "two", Name: "worker", Input: json.RawMessage(`{"input":"two"}`)},
		}}, registry, agent.WithToolConcurrency(2))
		outcome := make(chan error, 1)
		go func() { _, err := parent.Run(t.Context(), "parent"); outcome <- err }()
		await(t, provider.started)
		if provider.calls.Load() != 1 || provider.maxActive.Load() != 1 {
			t.Fatalf("same-adapter calls/max active before release = %d/%d", provider.calls.Load(), provider.maxActive.Load())
		}
		close(provider.release)
		if err := await(t, outcome); err != nil {
			t.Fatal(err)
		}
		if provider.calls.Load() != 2 || provider.maxActive.Load() != 1 {
			t.Fatalf("same-adapter calls/max active = %d/%d, want 2/1", provider.calls.Load(), provider.maxActive.Load())
		}
	})
	t.Run("separate adapters can overlap", func(t *testing.T) {
		provider := newBlockingProvider() // This test provider is safe for concurrent child Runs.
		child := agent.New(provider, tool.NewRegistry())
		first := agenttool.New(child, agenttool.Config{Name: "first"})
		second := agenttool.New(child, agenttool.Config{Name: "second"})
		registry := tool.NewRegistry()
		registry.MustRegister(first)
		registry.MustRegister(second)
		parent := agent.New(&toolCallsProvider{calls: []llm.ToolUseBlock{
			{ID: "one", Name: "first", Input: json.RawMessage(`{"input":"one"}`)},
			{ID: "two", Name: "second", Input: json.RawMessage(`{"input":"two"}`)},
		}}, registry, agent.WithToolConcurrency(2))
		outcome := make(chan error, 1)
		go func() { _, err := parent.Run(t.Context(), "parent"); outcome <- err }()
		await(t, provider.started)
		await(t, provider.started)
		if provider.maxActive.Load() != 2 {
			t.Fatalf("separate-adapter max active = %d, want 2", provider.maxActive.Load())
		}
		close(provider.release)
		if err := await(t, outcome); err != nil {
			t.Fatal(err)
		}
	})
}

func TestExecuteCancellationWhileQueued(t *testing.T) {
	provider := newBlockingProvider()
	adapter := agenttool.New(agent.New(provider, tool.NewRegistry()), agenttool.Config{Name: "worker"})
	first := make(chan error, 1)
	go func() { _, err := adapter.Execute(t.Context(), []byte(`{"input":"one"}`)); first <- err }()
	await(t, provider.started)
	base, cancel := context.WithCancel(t.Context())
	ctx := &doneObservedContext{Context: base, observed: make(chan struct{})}
	second := make(chan error, 1)
	go func() { _, err := adapter.Execute(ctx, []byte(`{"input":"two"}`)); second <- err }()
	await(t, ctx.observed)
	cancel()
	if err := await(t, second); !errors.Is(err, context.Canceled) || provider.calls.Load() != 1 {
		t.Fatalf("queued Execute = %v; child calls = %d", err, provider.calls.Load())
	}
	close(provider.release)
	if err := await(t, first); err != nil {
		t.Fatalf("first Execute: %v", err)
	}
}

func TestExecuteCancellationWhileRunningPropagatesContextValue(t *testing.T) {
	provider := newBlockingProvider()
	adapter := agenttool.New(agent.New(provider, tool.NewRegistry()), agenttool.Config{Name: "worker"})
	sentinel := &contextSentinel{label: "exact child context value"}
	base := context.WithValue(t.Context(), contextValueKey{}, sentinel)
	ctx, cancel := context.WithCancel(base)
	result := make(chan error, 1)
	go func() { _, err := adapter.Execute(ctx, []byte(`{"input":"task"}`)); result <- err }()
	await(t, provider.started)
	if got := await(t, provider.values); got != sentinel {
		t.Fatalf("child context value = %#v, want identical %#v", got, sentinel)
	}
	cancel()
	if err := await(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("running Execute = %v", err)
	}
}

type blockingProvider struct {
	started   chan struct{}
	release   chan struct{}
	values    chan any
	calls     atomic.Int32
	active    atomic.Int32
	maxActive atomic.Int32
}

func newBlockingProvider() *blockingProvider {
	return &blockingProvider{started: make(chan struct{}, 2), release: make(chan struct{}), values: make(chan any, 2)}
}
func (*blockingProvider) Name() string { return "blocking" }
func (*blockingProvider) Chat(context.Context, []llm.Message, []tool.ToolInfo, ...llm.Option) (*llm.Message, *llm.Usage, error) {
	return nil, nil, llm.ErrStreamingNotSupported
}
func (p *blockingProvider) ChatStream(ctx context.Context, _ []llm.Message, _ []tool.ToolInfo, _ ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	p.calls.Add(1)
	p.values <- ctx.Value(contextValueKey{})
	return func(yield func(llm.Chunk, error) bool) {
		active := p.active.Add(1)
		for max := p.maxActive.Load(); active > max && !p.maxActive.CompareAndSwap(max, active); max = p.maxActive.Load() {
		}
		defer p.active.Add(-1)
		p.started <- struct{}{}
		select {
		case <-p.release:
			yield(llm.TextDeltaChunk{Text: "done"}, nil)
		case <-ctx.Done():
			yield(nil, ctx.Err())
		}
	}, nil
}

type contextValueKey struct{}
type contextSentinel struct{ label string }

type doneObservedContext struct {
	context.Context
	observed chan struct{}
	once     sync.Once
}

func (c *doneObservedContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.observed) })
	return c.Context.Done()
}

func await[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	select {
	case value := <-ch:
		return value
	case <-ctx.Done():
		t.Fatal("timed out waiting for signal")
		var zero T
		return zero
	}
}
