package openairesponses

import (
	"testing"

	"github.com/dailz1/go-agent/llm"
)

func TestModelContextWindows(t *testing.T) {
	windows := map[string]int{"m-big": 200000, "default-model": 8192}
	p := NewProvider("key", "default-model", WithModelContextWindows(windows))
	if got, ok := p.ContextWindow("m-big"); !ok || got != 200000 {
		t.Fatalf("ContextWindow(m-big) = %d,%v", got, ok)
	}
	if got, ok := p.ContextWindow(""); !ok || got != 8192 {
		t.Fatalf("ContextWindow(empty->default) = %d,%v", got, ok)
	}
	if got, ok := p.ContextWindow("default-model"); !ok || got != 8192 {
		t.Fatalf("ContextWindow(default-model) = %d,%v", got, ok)
	}
	if _, ok := p.ContextWindow("unknown"); ok {
		t.Fatal("unknown model must be undeclared")
	}
	windows["m-big"] = 1
	if got, _ := p.ContextWindow("m-big"); got != 200000 {
		t.Fatalf("registration map aliased: got %d", got)
	}
	p2 := NewProvider("key", "m", WithModelContextWindows(map[string]int{"m": -5}))
	if _, ok := p2.ContextWindow("m"); ok {
		t.Fatal("non-positive entry must be undeclared")
	}
	p3 := NewProvider("key", "m")
	if _, ok := p3.ContextWindow("m"); ok {
		t.Fatal("unregistered provider must be undeclared")
	}
}

func TestDefaultModelName(t *testing.T) {
	p := NewProvider("key", "default-model")
	reporter, ok := any(p).(llm.DefaultModelReporter)
	if !ok {
		t.Fatal("provider must implement llm.DefaultModelReporter")
	}
	if got := reporter.DefaultModelName(); got != "default-model" {
		t.Fatalf("DefaultModelName = %q, want constructor default", got)
	}
	if got := NewProvider("key", "").DefaultModelName(); got != "" {
		t.Fatalf("empty constructor default = %q, want empty", got)
	}
	if got := NewProvider("key", " Mixed-Case ").DefaultModelName(); got != " Mixed-Case " {
		t.Fatalf("name must not be normalized: %q", got)
	}
}
