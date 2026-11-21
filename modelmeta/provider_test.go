package modelmeta

import (
	"context"
	"errors"
	"iter"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

type stubProvider struct {
	window func(string) (int, bool)
	chat   func(context.Context, []llm.Message, []tool.ToolInfo, ...llm.Option) (*llm.Message, *llm.Usage, error)
	stream func(context.Context, []llm.Message, []tool.ToolInfo, ...llm.Option) (iter.Seq2[llm.Chunk, error], error)
}

func (p *stubProvider) Name() string { return "stub" }
func (p *stubProvider) ContextWindow(model string) (int, bool) {
	if p.window != nil {
		return p.window(model)
	}
	return 1000, true
}
func (p *stubProvider) Chat(ctx context.Context, m []llm.Message, tools []tool.ToolInfo, opts ...llm.Option) (*llm.Message, *llm.Usage, error) {
	return p.chat(ctx, m, tools, opts...)
}
func (p *stubProvider) ChatStream(ctx context.Context, m []llm.Message, tools []tool.ToolInfo, opts ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	return p.stream(ctx, m, tools, opts...)
}

func TestGuardArgumentsAndAge(t *testing.T) {
	c := testCatalog()
	var typedNil *stubProvider
	for _, tc := range []struct {
		name string
		c    *Catalog
		p    llm.Provider
		age  time.Duration
	}{
		{"nil catalog", nil, &stubProvider{}, 0},
		{"zero catalog", &Catalog{}, &stubProvider{}, 0},
		{"nil provider", c, nil, 0},
		{"typed nil", c, typedNil, 0},
		{"windowless", c, struct{ llm.Provider }{&stubProvider{}}, 0},
		{"negative age", c, &stubProvider{}, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.c.Guard(tc.p, tc.age)
			if got != nil || !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("guard = %v, %v", got, err)
			}
		})
	}
	for _, tc := range []struct {
		name string
		age  time.Duration
		at   time.Time
		ok   bool
	}{
		{"before verification", time.Hour, fixedAt.Add(-1), false},
		{"fresh", time.Hour, fixedAt, true},
		{"before expiry", time.Hour, fixedAt.Add(time.Hour - 1), true},
		{"exact expiry", time.Hour, fixedAt.Add(time.Hour), false},
		{"after expiry", time.Hour, fixedAt.Add(time.Hour + 1), false},
		{"default fresh", 0, fixedAt.Add(DefaultMaxAge - 1), true},
		{"default expired", 0, fixedAt.Add(DefaultMaxAge), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setClock(t, func() time.Time { return tc.at })
			calls := 0
			p, err := c.Guard(&stubProvider{window: func(model string) (int, bool) {
				calls++
				if model != "exact-model" {
					t.Fatalf("model changed: %q", model)
				}
				return 1234, true
			}}, tc.age)
			if err != nil || p == nil {
				t.Fatalf("guard construction = %v, %v", p, err)
			}
			n, ok := p.(llm.ContextWindowProvider).ContextWindow("exact-model")
			if ok != tc.ok || (tc.ok && (n != 1234 || calls != 1)) || (!tc.ok && (n != 0 || calls != 0)) {
				t.Fatalf("window = %d, %v; delegate calls %d", n, ok, calls)
			}
		})
	}
}

// reportingStubProvider adds the issue #23 diagnostic capability to stubProvider.
type reportingStubProvider struct {
	*stubProvider
	report string
}

func (p *reportingStubProvider) DefaultModelName() string { return p.report }

func TestGuardForwardsDefaultModelNameAcrossAges(t *testing.T) {
	c := testCatalog()
	for _, tc := range []struct {
		name       string
		at         time.Time
		wantWindow bool
	}{
		{"fresh", fixedAt, true},
		{"exact expiry", fixedAt.Add(time.Hour), false},
		{"before verification", fixedAt.Add(-time.Second), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setClock(t, func() time.Time { return tc.at })
			windowCalls := 0
			base := &reportingStubProvider{stubProvider: &stubProvider{window: func(string) (int, bool) {
				windowCalls++
				return 1000, true
			}}, report: "base-default"}
			p, err := c.Guard(base, time.Hour)
			if err != nil {
				t.Fatalf("guard: %v", err)
			}
			reporter, ok := p.(llm.DefaultModelReporter)
			if !ok {
				t.Fatal("guard must satisfy llm.DefaultModelReporter")
			}
			if _, ok := p.(llm.ContextWindowProvider).ContextWindow("m"); ok != tc.wantWindow {
				t.Fatalf("ContextWindow ok = %v, want %v", ok, tc.wantWindow)
			}
			before := windowCalls
			clockReads := 0
			setClock(t, func() time.Time {
				clockReads++
				return tc.at
			})
			if got := reporter.DefaultModelName(); got != "base-default" {
				t.Fatalf("DefaultModelName = %q", got)
			}
			if clockReads != 0 {
				t.Fatalf("DefaultModelName read the clock %d times, want 0", clockReads)
			}
			if windowCalls != before {
				t.Fatalf("DefaultModelName consulted the window delegate (%d -> %d)", before, windowCalls)
			}
		})
	}
}

func TestGuardDefaultModelNameDegradation(t *testing.T) {
	c := testCatalog()
	p, err := c.Guard(&stubProvider{}, time.Hour)
	if err != nil {
		t.Fatalf("guard: %v", err)
	}
	if got := p.(llm.DefaultModelReporter).DefaultModelName(); got != "" {
		t.Fatalf("capability-less base = %q, want empty", got)
	}
	p, err = c.Guard(&reportingStubProvider{stubProvider: &stubProvider{}}, time.Hour)
	if err != nil {
		t.Fatalf("guard: %v", err)
	}
	if got := p.(llm.DefaultModelReporter).DefaultModelName(); got != "" {
		t.Fatalf("empty report = %q, want empty", got)
	}
	base := &reportingStubProvider{stubProvider: &stubProvider{}, report: "base-default"}
	wrapped, err := llm.WithContextWindows(base, map[string]int{"m": 1000})
	if err != nil {
		t.Fatalf("WithContextWindows: %v", err)
	}
	p, err = c.Guard(wrapped, time.Hour)
	if err != nil {
		t.Fatalf("guard over wrapper: %v", err)
	}
	if got := p.(llm.DefaultModelReporter).DefaultModelName(); got != "" {
		t.Fatalf("cross-layer read = %q, want empty", got)
	}
}

func TestGuardSamplesAtQueryStartAndReadsConcurrently(t *testing.T) {
	at := fixedAt
	setClock(t, func() time.Time { return at })
	c := testCatalog()
	p, err := c.Guard(&stubProvider{window: func(model string) (int, bool) {
		at = fixedAt.Add(time.Hour)
		return 123, true
	}}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	w := p.(llm.ContextWindowProvider)
	if n, ok := w.ContextWindow(""); !ok || n != 123 {
		t.Fatalf("in-flight query changed on expiry: %d, %v", n, ok)
	}
	if n, ok := w.ContextWindow(""); ok || n != 0 {
		t.Fatalf("next query did not expire: %d, %v", n, ok)
	}
	// The clock is now fixed and no delegate is called by an expired wrapper.
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			for range 20 {
				if n, ok := w.ContextWindow("m"); n != 0 || ok {
					t.Errorf("concurrent expired query = %d, %v", n, ok)
				}
			}
		})
	}
	wg.Wait()
}

func TestGuardDelegatesCallsWithoutConsumingStream(t *testing.T) {
	setClock(t, func() time.Time { t.Fatal("model calls sampled clock"); return time.Time{} })
	ctx := t.Context()
	messages := []llm.Message{llm.UserMessage("hello")}
	tools := []tool.ToolInfo{{Name: "tool"}}
	opts := []llm.Option{llm.WithModel("chosen")}
	message := llm.AssistantMessage("answer")
	usage := &llm.Usage{InputTokens: 12}
	callErr := errors.New("original chat error")
	streamErr := errors.New("original stream setup error")
	consumed, calls := 0, 0
	check := func(got context.Context, m []llm.Message, ts []tool.ToolInfo, os []llm.Option) {
		t.Helper()
		if got != ctx || &m[0] != &messages[0] || &ts[0] != &tools[0] || !reflect.DeepEqual(llm.ApplyOptions(os), llm.ApplyOptions(opts)) {
			t.Fatal("delegation changed request arguments")
		}
	}
	base := &stubProvider{
		chat: func(c context.Context, m []llm.Message, ts []tool.ToolInfo, os ...llm.Option) (*llm.Message, *llm.Usage, error) {
			check(c, m, ts, os)
			return &message, usage, callErr
		},
		stream: func(c context.Context, m []llm.Message, ts []tool.ToolInfo, os ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
			check(c, m, ts, os)
			calls++
			if calls == 2 {
				return nil, streamErr
			}
			return func(yield func(llm.Chunk, error) bool) {
				for range 3 {
					consumed++
					if !yield(llm.TextDeltaChunk{Text: "part"}, nil) {
						return
					}
				}
			}, nil
		},
	}
	p, err := testCatalog().Guard(base, 0)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name() != "stub" {
		t.Fatalf("name = %q", p.Name())
	}
	m, u, err := p.Chat(ctx, messages, tools, opts...)
	if m != &message || u != usage || err != callErr {
		t.Fatalf("chat returned different results: %v, %v, %v", m, u, err)
	}
	stream, err := p.ChatStream(ctx, messages, tools, opts...)
	if err != nil || consumed != 0 || calls != 1 {
		t.Fatalf("stream setup = %v, consumed %d, calls %d", err, consumed, calls)
	}
	for range stream {
		break
	}
	if consumed != 1 {
		t.Fatalf("early break consumed %d chunks", consumed)
	}
	if seq, err := p.ChatStream(ctx, messages, tools, opts...); seq != nil || err != streamErr {
		t.Fatalf("stream error changed: %v", err)
	}
}
