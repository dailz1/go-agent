package llm

import (
	"context"
	"errors"
	"iter"
	"testing"

	"github.com/dailz1/go-agent/tool"
)

type stubProvider struct{}

func (stubProvider) Name() string { return "stub" }

func (stubProvider) Chat(context.Context, []Message, []tool.ToolInfo, ...Option) (*Message, *Usage, error) {
	return nil, nil, errors.New("not scripted")
}

func (stubProvider) ChatStream(context.Context, []Message, []tool.ToolInfo, ...Option) (iter.Seq2[Chunk, error], error) {
	return nil, errors.New("not scripted")
}

func TestWithContextWindowsNilProvider(t *testing.T) {
	if _, err := WithContextWindows(nil, map[string]int{"m": 1}); err == nil {
		t.Fatal("nil provider accepted")
	}
}

func TestWithContextWindowsFiltersNonPositive(t *testing.T) {
	p, err := WithContextWindows(stubProvider{}, map[string]int{"m": 8000, "bad": 0, "worse": -1})
	if err != nil {
		t.Fatalf("WithContextWindows: %v", err)
	}
	cp, ok := p.(ContextWindowProvider)
	if !ok {
		t.Fatal("wrapped provider lost ContextWindowProvider")
	}
	if got, ok := cp.ContextWindow("m"); !ok || got != 8000 {
		t.Fatalf("ContextWindow(m) = %d,%v", got, ok)
	}
	if _, ok := cp.ContextWindow("bad"); ok {
		t.Fatal("non-positive entry must be undeclared")
	}
}

func TestWithContextWindowsEmptyReturnsOriginal(t *testing.T) {
	base := stubProvider{}
	p, err := WithContextWindows(base, nil)
	if err != nil {
		t.Fatalf("WithContextWindows: %v", err)
	}
	if _, ok := p.(ContextWindowProvider); ok {
		t.Fatal("empty registration must not claim the capability")
	}
	p, err = WithContextWindows(base, map[string]int{"m": -3})
	if err != nil {
		t.Fatalf("WithContextWindows: %v", err)
	}
	if _, ok := p.(ContextWindowProvider); ok {
		t.Fatal("fully filtered registration must not claim the capability")
	}
}

func TestContextWindowWrapperEmptyModelKey(t *testing.T) {
	p, err := WithContextWindows(stubProvider{}, map[string]int{"": 4096, "m": 8000})
	if err != nil {
		t.Fatalf("WithContextWindows: %v", err)
	}
	cp := p.(ContextWindowProvider)
	if got, ok := cp.ContextWindow(""); !ok || got != 4096 {
		t.Fatalf("ContextWindow(empty) = %d,%v", got, ok)
	}
	if got, ok := cp.ContextWindow("m"); !ok || got != 8000 {
		t.Fatalf("ContextWindow(m) = %d,%v", got, ok)
	}
	if _, ok := cp.ContextWindow("other"); ok {
		t.Fatal("unknown model must be undeclared")
	}
}

type reportingStubProvider struct {
	stubProvider
	report string
	calls  int
}

func (p *reportingStubProvider) DefaultModelName() string {
	p.calls++
	return p.report
}

func TestWithContextWindowsWrappingDropsReporter(t *testing.T) {
	base := &reportingStubProvider{report: "base-default"}
	p, err := WithContextWindows(base, map[string]int{"base-default": 4096})
	if err != nil {
		t.Fatalf("WithContextWindows: %v", err)
	}
	if _, ok := p.(DefaultModelReporter); ok {
		t.Fatal("actual wrapper must not claim DefaultModelReporter")
	}
	cp := p.(ContextWindowProvider)
	if _, ok := cp.ContextWindow(""); ok {
		t.Fatal("empty selector must not resolve through the wrapped provider default")
	}
	if got, ok := cp.ContextWindow("base-default"); !ok || got != 4096 {
		t.Fatalf("ContextWindow(base-default) = %d,%v", got, ok)
	}
	if base.calls != 0 {
		t.Fatalf("DefaultModelName calls = %d, want 0", base.calls)
	}
}

func TestWithContextWindowsEmptyReturnsOriginalPreservesReporter(t *testing.T) {
	base := &reportingStubProvider{report: "base-default"}
	for _, windows := range []map[string]int{nil, {}, {"m": -1}} {
		p, err := WithContextWindows(base, windows)
		if err != nil {
			t.Fatalf("WithContextWindows: %v", err)
		}
		if p != Provider(base) {
			t.Fatal("ineffective registration must return the original provider object")
		}
		reporter, ok := p.(DefaultModelReporter)
		if !ok {
			t.Fatal("original provider must keep DefaultModelReporter")
		}
		if got := reporter.DefaultModelName(); got != "base-default" {
			t.Fatalf("DefaultModelName = %q", got)
		}
		if _, ok := p.(ContextWindowProvider); ok {
			t.Fatal("ineffective registration must not claim the window capability")
		}
	}
}
