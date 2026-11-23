package agent

// Kernel behavior tests for the effective context window (issue #17,
// docs/DESIGN.md P1「模型上下文窗口声明与有效窗口」): a thread that
// hot-switches to a provider-declared smaller model must retarget
// compaction before the request goes out, and every budget derived from
// the window must follow the round's effective W.

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/llm/codex"
	"github.com/dailz1/go-agent/llm/codex/auth"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

type windowedProvider struct {
	name         string
	windows      map[string]int
	noCapability bool
	steps        [][]llm.Chunk
	received     [][]llm.Message
}

func (p *windowedProvider) Name() string { return p.name }

func (p *windowedProvider) ContextWindow(model string) (int, bool) {
	if p.noCapability || model == "" {
		return 0, false
	}
	tokens, ok := p.windows[model]
	return tokens, ok
}

func (p *windowedProvider) Chat(context.Context, []llm.Message, []tool.ToolInfo, ...llm.Option) (*llm.Message, *llm.Usage, error) {
	return nil, nil, llm.ErrStreamingNotSupported
}

func (p *windowedProvider) ChatStream(_ context.Context, messages []llm.Message, _ []tool.ToolInfo, _ ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	p.received = append(p.received, append([]llm.Message(nil), messages...))
	if len(p.steps) == 0 {
		return nil, fmt.Errorf("unexpected ChatStream call on %s", p.name)
	}
	chunks := p.steps[0]
	p.steps = p.steps[1:]
	return func(yield func(llm.Chunk, error) bool) {
		for _, chunk := range chunks {
			if !yield(chunk, nil) {
				return
			}
		}
	}, nil
}

func windowedText(text string) []llm.Chunk {
	return []llm.Chunk{&llm.TextDeltaChunk{Text: text}, &llm.DoneChunk{FinishReason: "stop"}}
}

func requestRunes(messages []llm.Message) int {
	total := 0
	for _, m := range messages {
		for _, b := range m.Content {
			if tb, ok := b.(*llm.TextBlock); ok {
				total += utf8.RuneCountInString(tb.Text)
			}
		}
	}
	return total
}

func windowPtr(s string) *string { return &s }

func TestContextWindowShrinkCompactsBeforeRequest(t *testing.T) {
	ctx := context.Background()
	pa := &windowedProvider{name: "pa", windows: map[string]int{"big-model": 100000},
		steps: [][]llm.Chunk{windowedText(strings.Repeat("a", 3000))}}
	pb := &windowedProvider{name: "pb", windows: map[string]int{"small-model": 2000},
		steps: [][]llm.Chunk{windowedText("small ok")}}
	st := store.NewMemory()
	ag := New(pa, tool.NewRegistry(),
		WithStore(st),
		WithProviders(pb),
		WithContextWindowTokens(100000),
		WithLLMOptions(llm.WithModel("big-model")),
		WithMaxIter(3),
	)

	if _, err := ag.Run(ctx, strings.Repeat("u", 1200), WithThread("t-w")); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if err := SetThreadConfig(ctx, st, "t-w", ConfigChange{
		Provider: windowPtr("pb"),
		Model:    windowPtr("small-model"),
	}); err != nil {
		t.Fatalf("switch: %v", err)
	}
	if _, err := ag.Run(ctx, "go", WithThread("t-w")); err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if len(pb.received) != 1 {
		t.Fatalf("pb calls = %d, want 1", len(pb.received))
	}
	if got := requestRunes(pb.received[0]); got > 1600 {
		t.Fatalf("request runes = %d, want compacted to <= target 1600", got)
	}
	if len(pb.received[0]) != 2 {
		t.Fatalf("request messages = %d, want uncompacted 3 shrunk to 2", len(pb.received[0]))
	}
}

func TestContextWindowGrowCappedByConstructor(t *testing.T) {
	ctx := context.Background()
	pa := &windowedProvider{name: "pa", windows: map[string]int{"big-model": 100000},
		steps: [][]llm.Chunk{windowedText("a")}}
	pb := &windowedProvider{name: "pb", windows: map[string]int{"huge-model": 1000000},
		steps: [][]llm.Chunk{windowedText("b")}}
	st := store.NewMemory()
	ag := New(pa, tool.NewRegistry(),
		WithStore(st),
		WithProviders(pb),
		WithContextWindowTokens(8192),
		WithLLMOptions(llm.WithModel("big-model")),
		WithMaxIter(3),
	)

	if _, err := ag.Run(ctx, "u", WithThread("t-g")); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if err := SetThreadConfig(ctx, st, "t-g", ConfigChange{
		Provider: windowPtr("pb"),
		Model:    windowPtr("huge-model"),
	}); err != nil {
		t.Fatalf("switch: %v", err)
	}
	if _, err := ag.Run(ctx, "go", WithThread("t-g")); err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if len(pb.received[0]) != 3 {
		t.Fatalf("request messages = %d, want full uncompacted history 3", len(pb.received[0]))
	}
}

func TestSameProviderModelSwitchRetargets(t *testing.T) {
	ctx := context.Background()
	pm := &windowedProvider{name: "pm", windows: map[string]int{"big-model": 100000, "small-model": 2000},
		steps: [][]llm.Chunk{
			windowedText(strings.Repeat("a", 3000)),
			windowedText("small ok"),
		}}
	st := store.NewMemory()
	ag := New(pm, tool.NewRegistry(),
		WithStore(st),
		WithContextWindowTokens(100000),
		WithLLMOptions(llm.WithModel("big-model")),
		WithMaxIter(3),
	)

	if _, err := ag.Run(ctx, strings.Repeat("u", 1200), WithThread("t-m")); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if err := SetThreadConfig(ctx, st, "t-m", ConfigChange{Model: windowPtr("small-model")}); err != nil {
		t.Fatalf("switch: %v", err)
	}
	if _, err := ag.Run(ctx, "go", WithThread("t-m")); err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if len(pm.received) != 2 {
		t.Fatalf("pm calls = %d, want 2", len(pm.received))
	}
	if got := requestRunes(pm.received[1]); got > 1600 {
		t.Fatalf("request runes = %d, want compacted to <= target 1600", got)
	}
}

func TestUndeclaredWindowFallsBack(t *testing.T) {
	ctx := context.Background()
	pa := &windowedProvider{name: "pa", noCapability: true,
		steps: [][]llm.Chunk{windowedText("a")}}
	st := store.NewMemory()
	ag := New(pa, tool.NewRegistry(),
		WithStore(st),
		WithContextWindowTokens(8192),
		WithMaxIter(3),
	)
	res, err := ag.Run(ctx, strings.Repeat("u", 100), WithThread("t-u"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(pa.received) != 1 || len(pa.received[0]) != 1 {
		t.Fatalf("history must pass through unchanged, got %d messages", len(pa.received[0]))
	}
	if res.Message.Content == nil {
		t.Fatal("missing final message")
	}
}

func TestCarryCappedAtEffectiveWindow(t *testing.T) {
	ctx := context.Background()
	pa := &windowedProvider{name: "pa", windows: map[string]int{"big-model": 100000},
		steps: [][]llm.Chunk{windowedText("a")}}
	pb := &windowedProvider{name: "pb", windows: map[string]int{"small-model": 2000},
		steps: [][]llm.Chunk{windowedText("injected ok")}}
	st := store.NewMemory()
	ag := New(pa, tool.NewRegistry(),
		WithStore(st),
		WithProviders(pb),
		WithContextWindowTokens(100000),
		WithLLMOptions(llm.WithModel("big-model")),
		WithMaxIter(3),
	)

	if _, err := ag.EnqueueInputWithID(ctx, "t-c", "k1", strings.Repeat("s", 50000)); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := ag.Run(ctx, "start", WithThread("t-c")); err != nil {
		t.Fatalf("run 1 (big-window drain): %v", err)
	}
	if err := SetThreadConfig(ctx, st, "t-c", ConfigChange{
		Provider: windowPtr("pb"),
		Model:    windowPtr("small-model"),
	}); err != nil {
		t.Fatalf("switch: %v", err)
	}
	if _, err := ag.Run(ctx, "go", WithThread("t-c")); err != nil {
		t.Fatalf("run 2 (shrink round): %v", err)
	}
	if got := requestRunes(pb.received[0]); got > 2000 {
		t.Fatalf("shrink round request = %d runes, must stay within W 2000", got)
	}
	_, err := ag.Run(ctx, strings.Repeat("n", 2500), WithThread("t-c"))
	if !errors.Is(err, ErrCompactionBudgetExceeded) {
		t.Fatalf("run 3: want fail-fast %v on oversized protected history under stale carry, got %v", ErrCompactionBudgetExceeded, err)
	}
}

// ---- issue #23: undeclared-window warning attribution ----

const undeclaredWindowMessage = "provider context window undeclared; using constructor-level window"

// stubBase supplies the llm.Provider core methods without any optional capability.
type stubBase struct{ name string }

func (p stubBase) Name() string { return p.name }

func (stubBase) Chat(context.Context, []llm.Message, []tool.ToolInfo, ...llm.Option) (*llm.Message, *llm.Usage, error) {
	return nil, nil, llm.ErrStreamingNotSupported
}

func (stubBase) ChatStream(context.Context, []llm.Message, []tool.ToolInfo, ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	return nil, llm.ErrStreamingNotSupported
}

type dualCapabilityProvider struct {
	stubBase
	window      func(string) (int, bool)
	seen        []string
	report      string
	reportCalls atomic.Int64
}

func (p *dualCapabilityProvider) ContextWindow(model string) (int, bool) {
	p.seen = append(p.seen, model)
	if p.window != nil {
		return p.window(model)
	}
	return 0, false
}

func (p *dualCapabilityProvider) DefaultModelName() string {
	p.reportCalls.Add(1)
	return p.report
}

type windowOnlyProvider struct {
	stubBase
	window func(string) (int, bool)
	seen   []string
}

func (p *windowOnlyProvider) ContextWindow(model string) (int, bool) {
	p.seen = append(p.seen, model)
	if p.window != nil {
		return p.window(model)
	}
	return 0, false
}

type reportOnlyProvider struct {
	stubBase
	report      string
	reportCalls atomic.Int64
}

func (p *reportOnlyProvider) DefaultModelName() string {
	p.reportCalls.Add(1)
	return p.report
}

func windowLookup(windows map[string]int) func(string) (int, bool) {
	return func(model string) (int, bool) {
		tokens, ok := windows[model]
		return tokens, ok
	}
}

func warnAttrString(t *testing.T, record truncationLogRecord, key string) string {
	t.Helper()
	v, ok := record.attrs[key]
	if !ok {
		t.Fatalf("missing log attr %q in %q", key, record.message)
	}
	if v.Kind() != slog.KindString {
		t.Fatalf("log attr %q has kind %s, want string", key, v.Kind())
	}
	return v.String()
}

func warnRecordModels(t *testing.T, capture *truncationLogCapture) []string {
	t.Helper()
	records := capture.recordsWithMessage(undeclaredWindowMessage)
	models := make([]string, 0, len(records))
	for _, record := range records {
		models = append(models, warnAttrString(t, record, "model"))
	}
	return models
}

func newAttributionAgent(t *testing.T, handler slog.Handler) *Agent {
	t.Helper()
	return New(stubBase{name: "anchor"}, tool.NewRegistry(),
		WithContextWindowTokens(8192), WithLogger(slog.New(handler)))
}

func TestUndeclaredWindowWarnAttributesDefaultModel(t *testing.T) {
	undeclared := func(string) (int, bool) { return 0, false }

	for _, tc := range []struct {
		name            string
		newProv         func() llm.Provider
		opts            []llm.Option
		wantWindow      int
		wantModels      []string
		wantProvider    string
		wantReportCalls int64
		wantSeen        []string
	}{
		{
			name: "default undeclared",
			newProv: func() llm.Provider {
				return &dualCapabilityProvider{stubBase: stubBase{name: "dual"}, report: "A", window: undeclared}
			},
			wantWindow: 8192, wantModels: []string{"A"}, wantProvider: "dual",
			wantReportCalls: 1, wantSeen: []string{""},
		},
		{
			name: "zero declaration",
			newProv: func() llm.Provider {
				return &dualCapabilityProvider{stubBase: stubBase{name: "dual"}, report: "A",
					window: func(string) (int, bool) { return 0, true }}
			},
			wantWindow: 8192, wantModels: []string{"A"}, wantProvider: "dual",
			wantReportCalls: 1, wantSeen: []string{""},
		},
		{
			name: "negative declaration",
			newProv: func() llm.Provider {
				return &dualCapabilityProvider{stubBase: stubBase{name: "dual"}, report: "A",
					window: func(string) (int, bool) { return -1, true }}
			},
			wantWindow: 8192, wantModels: []string{"A"}, wantProvider: "dual",
			wantReportCalls: 1, wantSeen: []string{""},
		},
		{
			name: "default window declared",
			newProv: func() llm.Provider {
				return &dualCapabilityProvider{stubBase: stubBase{name: "dual"}, report: "A",
					window: windowLookup(map[string]int{"": 4096})}
			},
			wantWindow: 4096, wantModels: nil, wantProvider: "dual",
			wantReportCalls: 0, wantSeen: []string{""},
		},
		{
			name: "declaration above constructor",
			newProv: func() llm.Provider {
				return &dualCapabilityProvider{stubBase: stubBase{name: "dual"}, report: "A",
					window: windowLookup(map[string]int{"": 16384})}
			},
			wantWindow: 8192, wantModels: nil, wantProvider: "dual",
			wantReportCalls: 0, wantSeen: []string{""},
		},
		{
			name: "explicit selector keeps its name",
			newProv: func() llm.Provider {
				return &dualCapabilityProvider{stubBase: stubBase{name: "dual"}, report: "A", window: undeclared}
			},
			opts:       []llm.Option{llm.WithModel("B")},
			wantWindow: 8192, wantModels: []string{"B"}, wantProvider: "dual",
			wantReportCalls: 0, wantSeen: []string{"B"},
		},
		{
			name: "reporter returns empty",
			newProv: func() llm.Provider {
				return &dualCapabilityProvider{stubBase: stubBase{name: "dual"}, window: undeclared}
			},
			wantWindow: 8192, wantModels: []string{""}, wantProvider: "dual",
			wantReportCalls: 1, wantSeen: []string{""},
		},
		{
			name: "window capability only",
			newProv: func() llm.Provider {
				return &windowOnlyProvider{stubBase: stubBase{name: "wonly"}, window: undeclared}
			},
			wantWindow: 8192, wantModels: []string{""}, wantProvider: "wonly",
			wantReportCalls: 0, wantSeen: []string{""},
		},
		{
			name: "reporter capability only",
			newProv: func() llm.Provider {
				return &reportOnlyProvider{stubBase: stubBase{name: "ronly"}, report: "A"}
			},
			wantWindow: 8192, wantModels: []string{"A"}, wantProvider: "ronly",
			wantReportCalls: 1,
		},
		{
			name:       "no capabilities",
			newProv:    func() llm.Provider { return stubBase{name: "plain"} },
			wantWindow: 8192, wantModels: []string{""}, wantProvider: "plain",
			wantReportCalls: 0,
		},
		{
			name: "second resolution forbidden",
			newProv: func() llm.Provider {
				return &dualCapabilityProvider{stubBase: stubBase{name: "dual"}, report: "A",
					window: windowLookup(map[string]int{"A": 4096})}
			},
			wantWindow: 8192, wantModels: []string{"A"}, wantProvider: "dual",
			wantReportCalls: 1, wantSeen: []string{""},
		},
		{
			name:       "nil provider",
			newProv:    func() llm.Provider { return nil },
			wantWindow: 8192, wantModels: nil, wantProvider: "",
			wantReportCalls: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture := &truncationLogCapture{}
			a := newAttributionAgent(t, capture)
			prov := tc.newProv()
			got := a.roundContextWindow(prov, tc.opts)
			if got != tc.wantWindow {
				t.Fatalf("window = %d, want %d", got, tc.wantWindow)
			}
			models := warnRecordModels(t, capture)
			if len(models) == 0 && len(tc.wantModels) == 0 {
				// no records expected; fall through to counters
			} else if !slices.Equal(models, tc.wantModels) {
				t.Fatalf("warn models = %q, want %q", models, tc.wantModels)
			}
			for _, record := range capture.recordsWithMessage(undeclaredWindowMessage) {
				if got := warnAttrString(t, record, "provider"); got != tc.wantProvider {
					t.Fatalf("warn provider = %q, want %q", got, tc.wantProvider)
				}
				tokens, ok := record.attrs["window_tokens"]
				if !ok {
					t.Fatalf("missing log attr window_tokens in %q", record.message)
				}
				if got := tokens.Int64(); got != int64(tc.wantWindow) || tokens.Kind() != slog.KindInt64 {
					t.Fatalf("warn window_tokens = %d (kind %s), want int64 %d", got, tokens.Kind(), tc.wantWindow)
				}
			}
			switch p := prov.(type) {
			case *dualCapabilityProvider:
				if p.reportCalls.Load() != tc.wantReportCalls {
					t.Fatalf("DefaultModelName calls = %d, want %d", p.reportCalls.Load(), tc.wantReportCalls)
				}
				if !slices.Equal(p.seen, tc.wantSeen) {
					t.Fatalf("ContextWindow selectors = %q, want %q", p.seen, tc.wantSeen)
				}
			case *windowOnlyProvider:
				if !slices.Equal(p.seen, tc.wantSeen) {
					t.Fatalf("ContextWindow selectors = %q, want %q", p.seen, tc.wantSeen)
				}
			case *reportOnlyProvider:
				if p.reportCalls.Load() != tc.wantReportCalls {
					t.Fatalf("DefaultModelName calls = %d, want %d", p.reportCalls.Load(), tc.wantReportCalls)
				}
			}
		})
	}
}

type attributingRunProvider struct {
	stubBase
	report       string
	capturedOpts [][]llm.Option
	steps        [][]llm.Chunk
}

func (p *attributingRunProvider) DefaultModelName() string { return p.report }

func (p *attributingRunProvider) ChatStream(_ context.Context, _ []llm.Message, _ []tool.ToolInfo, opts ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	p.capturedOpts = append(p.capturedOpts, append([]llm.Option(nil), opts...))
	if len(p.steps) == 0 {
		return nil, fmt.Errorf("unexpected ChatStream call on %s", p.name)
	}
	chunks := p.steps[0]
	p.steps = p.steps[1:]
	return func(yield func(llm.Chunk, error) bool) {
		for _, chunk := range chunks {
			if !yield(chunk, nil) {
				return
			}
		}
	}, nil
}

func TestUndeclaredWindowAttributionLeavesRequestUntouched(t *testing.T) {
	ctx := context.Background()
	p := &attributingRunProvider{stubBase: stubBase{name: "runp"}, report: "glm-default",
		steps: [][]llm.Chunk{windowedText("ok")}}
	capture := &truncationLogCapture{}
	ag := New(p, tool.NewRegistry(), WithContextWindowTokens(8192),
		WithLogger(slog.New(capture)), WithMaxIter(3))
	if _, err := ag.Run(ctx, "u"); err != nil {
		t.Fatalf("run: %v", err)
	}
	models := warnRecordModels(t, capture)
	if len(models) != 1 || models[0] != "glm-default" {
		t.Fatalf("warn models = %q, want [glm-default]", models)
	}
	if len(p.capturedOpts) != 1 {
		t.Fatalf("requests = %d, want 1", len(p.capturedOpts))
	}
	if got := llm.ApplyOptions(p.capturedOpts[0]).Model; got != "" {
		t.Fatalf("request model = %q, want empty selector", got)
	}
}

func TestUndeclaredWindowWarnDedup(t *testing.T) {
	type attributionRound struct {
		prov llm.Provider
		opts []llm.Option
	}
	for _, tc := range []struct {
		name          string
		rounds        func() []attributionRound
		wantModels    []string
		wantProviders []string
	}{
		{
			name: "same default repeated warns once",
			rounds: func() []attributionRound {
				p := &dualCapabilityProvider{stubBase: stubBase{name: "p"}, report: "A"}
				return []attributionRound{{p, nil}, {p, nil}}
			},
			wantModels: []string{"A"}, wantProviders: []string{"p"},
		},
		{
			name: "default and explicit same name share one key",
			rounds: func() []attributionRound {
				p := &dualCapabilityProvider{stubBase: stubBase{name: "p"}, report: "A"}
				return []attributionRound{
					{p, nil},
					{p, []llm.Option{llm.WithModel("B")}},
					{p, nil},
					{p, []llm.Option{llm.WithModel("A")}},
				}
			},
			wantModels: []string{"A", "B"}, wantProviders: []string{"p", "p"},
		},
		{
			name: "same name different instances count apart",
			rounds: func() []attributionRound {
				p1 := &dualCapabilityProvider{stubBase: stubBase{name: "p"}, report: "A"}
				p2 := &dualCapabilityProvider{stubBase: stubBase{name: "p"}, report: "B"}
				return []attributionRound{{p1, nil}, {p2, nil}}
			},
			wantModels: []string{"A", "B"}, wantProviders: []string{"p", "p"},
		},
		{
			name: "provider switch keeps separate keys",
			rounds: func() []attributionRound {
				p := &dualCapabilityProvider{stubBase: stubBase{name: "p"}, report: "A"}
				q := &dualCapabilityProvider{stubBase: stubBase{name: "q"}, report: "A"}
				return []attributionRound{{p, nil}, {q, []llm.Option{llm.WithModel("B")}}}
			},
			wantModels: []string{"A", "B"}, wantProviders: []string{"p", "q"},
		},
		{
			name: "missing capability keeps the empty key",
			rounds: func() []attributionRound {
				p := &windowOnlyProvider{stubBase: stubBase{name: "wonly"}}
				return []attributionRound{{p, nil}, {p, nil}}
			},
			wantModels: []string{""}, wantProviders: []string{"wonly"},
		},
		{
			name: "same default name across providers counts apart",
			rounds: func() []attributionRound {
				p := &dualCapabilityProvider{stubBase: stubBase{name: "p"}, report: "A"}
				q := &dualCapabilityProvider{stubBase: stubBase{name: "q"}, report: "A"}
				return []attributionRound{{p, nil}, {q, nil}, {p, nil}}
			},
			wantModels: []string{"A", "A"}, wantProviders: []string{"p", "q"},
		},
		{
			name: "same name and report instances share one key",
			rounds: func() []attributionRound {
				p1 := &dualCapabilityProvider{stubBase: stubBase{name: "p"}, report: "A"}
				p2 := &dualCapabilityProvider{stubBase: stubBase{name: "p"}, report: "A"}
				return []attributionRound{{p1, nil}, {p2, nil}}
			},
			wantModels: []string{"A"}, wantProviders: []string{"p"},
		},
		{
			name: "empty report repeated keeps one empty entry",
			rounds: func() []attributionRound {
				p := &dualCapabilityProvider{stubBase: stubBase{name: "p"}}
				return []attributionRound{{p, nil}, {p, nil}}
			},
			wantModels: []string{""}, wantProviders: []string{"p"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture := &truncationLogCapture{}
			a := newAttributionAgent(t, capture)
			for _, round := range tc.rounds() {
				a.roundContextWindow(round.prov, round.opts)
			}
			records := capture.recordsWithMessage(undeclaredWindowMessage)
			models := make([]string, 0, len(records))
			providers := make([]string, 0, len(records))
			for _, record := range records {
				models = append(models, warnAttrString(t, record, "model"))
				providers = append(providers, warnAttrString(t, record, "provider"))
			}
			if !slices.Equal(models, tc.wantModels) {
				t.Fatalf("warn models = %q, want %q", models, tc.wantModels)
			}
			if !slices.Equal(providers, tc.wantProviders) {
				t.Fatalf("warn providers = %q, want %q", providers, tc.wantProviders)
			}
		})
	}

	t.Run("agents isolate their tables", func(t *testing.T) {
		prov := &dualCapabilityProvider{stubBase: stubBase{name: "p"}, report: "A"}
		for range 2 {
			capture := &truncationLogCapture{}
			a := newAttributionAgent(t, capture)
			a.roundContextWindow(prov, nil)
			if models := warnRecordModels(t, capture); len(models) != 1 || models[0] != "A" {
				t.Fatalf("warn models = %q, want [A] per agent", models)
			}
		}
	})
}

func TestUndeclaredWindowAgentWithActualWrapper(t *testing.T) {
	newBase := func() *dualCapabilityProvider {
		return &dualCapabilityProvider{stubBase: stubBase{name: "wrapped"}, report: "A"}
	}
	t.Run("empty-key declaration covers unattributed rounds", func(t *testing.T) {
		wrapped, err := llm.WithContextWindows(newBase(), map[string]int{"": 4096})
		if err != nil {
			t.Fatalf("WithContextWindows: %v", err)
		}
		capture := &truncationLogCapture{}
		a := newAttributionAgent(t, capture)
		if got := a.roundContextWindow(wrapped, nil); got != 4096 {
			t.Fatalf("window = %d, want 4096", got)
		}
		if models := warnRecordModels(t, capture); len(models) != 0 {
			t.Fatalf("warn models = %q, want none", models)
		}
	})
	t.Run("name-only declaration still warns with empty attribution", func(t *testing.T) {
		wrapped, err := llm.WithContextWindows(newBase(), map[string]int{"A": 4096})
		if err != nil {
			t.Fatalf("WithContextWindows: %v", err)
		}
		capture := &truncationLogCapture{}
		a := newAttributionAgent(t, capture)
		if got := a.roundContextWindow(wrapped, nil); got != 8192 {
			t.Fatalf("window = %d, want 8192", got)
		}
		models := warnRecordModels(t, capture)
		if len(models) != 1 || models[0] != "" {
			t.Fatalf("warn models = %q, want one empty attribution", models)
		}
	})
}

func TestUndeclaredWindowAttributionWithThreadConfig(t *testing.T) {
	ctx := context.Background()
	p := &attributingRunProvider{stubBase: stubBase{name: "tp"}, report: "glm-default",
		steps: [][]llm.Chunk{windowedText("first ok"), windowedText("second ok")}}
	st := store.NewMemory()
	capture := &truncationLogCapture{}
	ag := New(p, tool.NewRegistry(), WithStore(st), WithContextWindowTokens(8192),
		WithLogger(slog.New(capture)), WithMaxIter(3))
	if _, err := ag.Run(ctx, "u", WithThread("t-a23")); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if err := SetThreadConfig(ctx, st, "t-a23", ConfigChange{Model: windowPtr("explicit-b")}); err != nil {
		t.Fatalf("switch: %v", err)
	}
	if _, err := ag.Run(ctx, "go", WithThread("t-a23")); err != nil {
		t.Fatalf("run 2: %v", err)
	}
	models := warnRecordModels(t, capture)
	if !slices.Equal(models, []string{"glm-default", "explicit-b"}) {
		t.Fatalf("warn models = %q, want [glm-default explicit-b]", models)
	}
	if len(p.capturedOpts) != 2 {
		t.Fatalf("requests = %d, want 2", len(p.capturedOpts))
	}
	if got := llm.ApplyOptions(p.capturedOpts[0]).Model; got != "" {
		t.Fatalf("request 1 model = %q, want empty selector", got)
	}
	if got := llm.ApplyOptions(p.capturedOpts[1]).Model; got != "explicit-b" {
		t.Fatalf("request 2 model = %q, want explicit-b", got)
	}
}

type syncLogHandler struct {
	mu      sync.Mutex
	records []truncationLogRecord
}

func (h *syncLogHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelWarn
}

func (h *syncLogHandler) Handle(_ context.Context, record slog.Record) error {
	attrs := map[string]slog.Value{}
	record.Attrs(func(attr slog.Attr) bool {
		attrs[attr.Key] = attr.Value
		return true
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, truncationLogRecord{message: record.Message, attrs: attrs})
	return nil
}

func (h *syncLogHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }

func (h *syncLogHandler) WithGroup(_ string) slog.Handler { return h }

func TestUndeclaredWindowWarnOnceConcurrent(t *testing.T) {
	// reportOnlyProvider keeps every field it touches concurrency-safe
	// (atomic counter, no window bookkeeping), so under contention the
	// race detector sees only kernel state.
	p := &reportOnlyProvider{stubBase: stubBase{name: "cp"}, report: "A"}
	capture := &syncLogHandler{}
	a := New(p, tool.NewRegistry(), WithContextWindowTokens(8192), WithLogger(slog.New(capture)))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			<-start
			if w := a.roundContextWindow(p, nil); w != 8192 {
				t.Errorf("window = %d, want 8192", w)
			}
		})
	}
	close(start)
	wg.Wait()
	records := capture.records
	if len(records) != 1 {
		t.Fatalf("warn records = %d, want 1", len(records))
	}
	if got := warnAttrString(t, records[0], "model"); got != "A" {
		t.Fatalf("warn model = %q, want A", got)
	}
	if got := p.reportCalls.Load(); got != 16 {
		t.Fatalf("DefaultModelName calls = %d, want 16 (no caching)", got)
	}
}

type countingAuthSource struct {
	tokenCalls   atomic.Int64
	refreshCalls atomic.Int64
}

func (s *countingAuthSource) Token(context.Context) (auth.Token, error) {
	s.tokenCalls.Add(1)
	return auth.Token{}, errors.New("auth source must not be consulted before the model check")
}

func (s *countingAuthSource) Refresh(context.Context, *auth.Token) (auth.Token, error) {
	s.refreshCalls.Add(1)
	return auth.Token{}, errors.New("auth source must not be refreshed before the model check")
}

// codexOrderObserver pins the ordering contract: by the time the first
// request attempt enters ChatStream, the round must already carry the
// undeclared-window warning.
type codexOrderObserver struct {
	*codex.Provider
	capture              *truncationLogCapture
	sawWarnBeforeRequest bool
}

func (w *codexOrderObserver) ChatStream(ctx context.Context, messages []llm.Message, tools []tool.ToolInfo, opts ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	w.sawWarnBeforeRequest = len(w.capture.recordsWithMessage(undeclaredWindowMessage)) > 0
	return w.Provider.ChatStream(ctx, messages, tools, opts...)
}

func TestCodexEmptyModelWarnsThenFailsModelRequired(t *testing.T) {
	ctx := context.Background()
	authSource := &countingAuthSource{}
	capture := &truncationLogCapture{}
	p := &codexOrderObserver{Provider: codex.NewProvider("", codex.WithAuthSource(authSource)), capture: capture}
	ag := New(p, tool.NewRegistry(), WithContextWindowTokens(8192),
		WithLogger(slog.New(capture)), WithMaxIter(3))
	_, err := ag.Run(ctx, "u")
	var codexErr *codex.Error
	if !errors.As(err, &codexErr) || codexErr.Code != "model_required" || codexErr.Kind != codex.KindCapability {
		t.Fatalf("err = %v, want codex capability model_required", err)
	}
	models := warnRecordModels(t, capture)
	if len(models) != 1 || models[0] != "" {
		t.Fatalf("warn models = %q, want one empty attribution", models)
	}
	if !p.sawWarnBeforeRequest {
		t.Fatal("undeclared-window warning must exist before the first ChatStream attempt")
	}
	if got := authSource.tokenCalls.Load(); got != 0 {
		t.Fatalf("auth Token calls = %d, want 0 before the model check", got)
	}
	if got := authSource.refreshCalls.Load(); got != 0 {
		t.Fatalf("auth Refresh calls = %d, want 0 before the model check", got)
	}
}
