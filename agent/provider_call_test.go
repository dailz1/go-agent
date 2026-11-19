package agent

// Pins the issue #12 provider-request-history isolation contract: every
// direct provider invocation (ChatStream attempt, non-streaming fallback)
// receives a fresh deep copy of the round's live history, so a mutating or
// retaining third-party provider cannot poison the run. Foreign block types
// (whitelist outsiders) are carried through with outer-layer isolation only
// — the D0 carve-out, pinned in TestProviderForeignBlockCompatibility.

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

type scriptedStep struct {
	stream bool
	chunks []llm.Chunk
	msg    *llm.Message
	err    error
	mutate func([]llm.Message)
}

// scriptedProvider serves steps in call order and records two views per
// direct invocation: snapshots[i] is a copyMessages freeze taken at
// hand-over time BEFORE the scripted mutation runs (it shows what the
// kernel actually sent, so aliasing pollution is visible there), and
// refs[i] is the real retained reference the provider received.
type scriptedProvider struct {
	mu        sync.Mutex
	steps     []scriptedStep
	snapshots [][]llm.Message
	refs      [][]llm.Message
}

func (p *scriptedProvider) Name() string { return "scripted" }

func (p *scriptedProvider) take(stream bool, history []llm.Message) scriptedStep {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.steps) == 0 {
		panic(fmt.Sprintf("scriptedProvider: unexpected %s call (no steps left)", callModeName(stream)))
	}
	step := p.steps[0]
	if step.stream != stream {
		panic(fmt.Sprintf("scriptedProvider: step %d is a %s step, got %s call",
			len(p.snapshots), callModeName(step.stream), callModeName(stream)))
	}
	p.steps = p.steps[1:]
	p.snapshots = append(p.snapshots, deepCopyMessages(history))
	p.refs = append(p.refs, history)
	if step.mutate != nil {
		step.mutate(history)
	}
	return step
}

func callModeName(stream bool) string {
	if stream {
		return "ChatStream"
	}
	return "Chat"
}

func (p *scriptedProvider) ChatStream(_ context.Context, history []llm.Message, _ []tool.ToolInfo, _ ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	step := p.take(true, history)
	if step.err != nil {
		return nil, step.err
	}
	chunks := step.chunks
	return func(yield func(llm.Chunk, error) bool) {
		for _, chunk := range chunks {
			if !yield(chunk, nil) {
				return
			}
		}
	}, nil
}

func (p *scriptedProvider) Chat(_ context.Context, history []llm.Message, _ []tool.ToolInfo, _ ...llm.Option) (*llm.Message, *llm.Usage, error) {
	step := p.take(false, history)
	return step.msg, nil, step.err
}

func probeToolCallChunks() []llm.Chunk {
	return []llm.Chunk{
		llm.ToolCallStartChunk{Index: 0, ID: "n1", Name: "probe"},
		llm.ToolCallArgsChunk{Index: 0, Delta: `{}`},
		llm.DoneChunk{FinishReason: "tool_calls"},
	}
}

func probeTextChunks() []llm.Chunk {
	return []llm.Chunk{
		llm.TextDeltaChunk{Text: "final answer"},
		llm.DoneChunk{FinishReason: "stop"},
	}
}

type gatedTool struct {
	info     tool.ToolInfo
	entered  chan struct{}
	release  chan struct{}
	executes int
}

func (g *gatedTool) Info() tool.ToolInfo { return g.info }

func (g *gatedTool) Execute(_ context.Context, _ json.RawMessage) (*tool.ToolResult, error) {
	g.executes++
	if g.entered != nil {
		close(g.entered)
	}
	if g.release != nil {
		<-g.release
	}
	return tool.NewTextResult("gated output"), nil
}

func newIsolationAgent(p llm.Provider, maxRetries int, opts ...Option) *Agent {
	reg := tool.NewRegistry()
	reg.MustRegister(&gatedTool{info: tool.ToolInfo{Name: "probe", Description: "probe tool"}})
	all := append([]Option{
		WithRetryConfig(AgentRetryConfig{MaxRetries: maxRetries, BaseDelay: time.Nanosecond}),
		WithLogger(discardLogger()),
	}, opts...)
	return New(p, reg, all...)
}

// runIsolationRounds drives two streaming rounds: round 1 requests a tool
// call, round 2 finishes with text.
func runIsolationRounds(t *testing.T, seed []llm.Message, mutate func([]llm.Message)) (*scriptedProvider, *RunResult) {
	t.Helper()
	p := &scriptedProvider{steps: []scriptedStep{
		{stream: true, chunks: probeToolCallChunks(), mutate: mutate},
		{stream: true, chunks: probeTextChunks(), mutate: mutate},
	}}
	ag := newIsolationAgent(p, 0)
	result, err := ag.RunWithHistory(context.Background(), seed, "go")
	if err != nil {
		t.Fatalf("RunWithHistory() error = %v", err)
	}
	return p, result
}

func TestProviderInvocationHistoryIsolation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		seed   func() []llm.Message
		mutate func(h []llm.Message)
		verify func(t *testing.T, label string, h []llm.Message)
	}{
		{
			name: "value text content replace",
			seed: func() []llm.Message {
				return []llm.Message{llm.SystemMessage("system policy"), llm.UserMessage("user question")}
			},
			mutate: func(h []llm.Message) {
				h[1].Content[0] = llm.TextBlock{Type: "text", Text: "MUTATED"}
			},
			verify: func(t *testing.T, label string, h []llm.Message) {
				tb, ok := h[1].Content[0].(llm.TextBlock)
				if !ok || tb.Text != "user question" {
					t.Errorf("%s: user text = %#v, want untouched %q", label, h[1].Content[0], "user question")
				}
			},
		},
		{
			name: "role replace on outer message",
			seed: func() []llm.Message {
				return []llm.Message{llm.SystemMessage("system policy"), llm.UserMessage("user question")}
			},
			mutate: func(h []llm.Message) {
				h[1].Role = llm.RoleAssistant
			},
			verify: func(t *testing.T, label string, h []llm.Message) {
				if h[1].Role != llm.RoleUser {
					t.Errorf("%s: user role = %q, want %q", label, h[1].Role, llm.RoleUser)
				}
			},
		},
		{
			name: "pointer-form text block field write",
			seed: func() []llm.Message {
				return []llm.Message{
					{Role: llm.RoleSystem, Content: []llm.ContentBlock{&llm.TextBlock{Type: "text", Text: "system policy"}}},
					llm.UserMessage("user question"),
				}
			},
			mutate: func(h []llm.Message) {
				h[0].Content[0].(*llm.TextBlock).Text = "MUTATED"
			},
			verify: func(t *testing.T, label string, h []llm.Message) {
				if got := h[0].Content[0].(*llm.TextBlock).Text; got != "system policy" {
					t.Errorf("%s: system text = %q, want %q", label, got, "system policy")
				}
			},
		},
		{
			name: "tool use input bytes in place",
			seed: func() []llm.Message {
				return []llm.Message{
					llm.SystemMessage("system policy"),
					llm.UserMessage("user question"),
					llm.AssistantToolCallMessage(llm.ToolUseBlock{Type: "tool_use", ID: "seed1", Name: "probe", Input: json.RawMessage(`{"k":"v"}`)}),
				}
			},
			mutate: func(h []llm.Message) {
				tb := h[2].Content[0].(llm.ToolUseBlock)
				tb.Input[0] = 'Z'
			},
			verify: func(t *testing.T, label string, h []llm.Message) {
				tb := h[2].Content[0].(llm.ToolUseBlock)
				if len(tb.Input) == 0 || tb.Input[0] != '{' {
					t.Errorf("%s: tool use input = %q, want untouched JSON", label, string(tb.Input))
				}
			},
		},
		{
			name: "pointer-form tool use field write",
			seed: func() []llm.Message {
				return []llm.Message{
					llm.SystemMessage("system policy"),
					llm.UserMessage("user question"),
					{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
						&llm.ToolUseBlock{Type: "tool_use", ID: "seed1", Name: "probe", Input: json.RawMessage(`{"k":"v"}`)},
					}},
				}
			},
			mutate: func(h []llm.Message) {
				h[2].Content[0].(*llm.ToolUseBlock).Name = "MUTATED"
			},
			verify: func(t *testing.T, label string, h []llm.Message) {
				if got := h[2].Content[0].(*llm.ToolUseBlock).Name; got != "probe" {
					t.Errorf("%s: tool use name = %q, want %q", label, got, "probe")
				}
			},
		},
		{
			name: "reasoning item summary slice write",
			seed: func() []llm.Message {
				return []llm.Message{
					llm.SystemMessage("system policy"),
					llm.UserMessage("user question"),
					{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
						llm.ReasoningItemBlock{Type: "reasoning_item", Summary: []string{"keep reasoning"}},
					}},
				}
			},
			mutate: func(h []llm.Message) {
				ri := h[2].Content[0].(llm.ReasoningItemBlock)
				ri.Summary[0] = "MUTATED"
			},
			verify: func(t *testing.T, label string, h []llm.Message) {
				ri := h[2].Content[0].(llm.ReasoningItemBlock)
				if len(ri.Summary) != 1 || ri.Summary[0] != "keep reasoning" {
					t.Errorf("%s: reasoning summary = %#v, want untouched", label, ri.Summary)
				}
			},
		},
		{
			name: "image data bytes in place",
			seed: func() []llm.Message {
				return []llm.Message{
					llm.SystemMessage("system policy"),
					{Role: llm.RoleUser, Content: []llm.ContentBlock{
						llm.ImageBlock{Type: "image", MIMEType: "image/png", Data: []byte("PNGDATA")},
					}},
				}
			},
			mutate: func(h []llm.Message) {
				ib := h[1].Content[0].(llm.ImageBlock)
				ib.Data[0] = 'Z'
			},
			verify: func(t *testing.T, label string, h []llm.Message) {
				ib := h[1].Content[0].(llm.ImageBlock)
				if len(ib.Data) == 0 || ib.Data[0] != 'P' {
					t.Errorf("%s: image data = %q, want untouched", label, string(ib.Data))
				}
			},
		},
		{
			name: "tool result nested image data in place",
			seed: func() []llm.Message {
				return []llm.Message{
					llm.SystemMessage("system policy"),
					llm.UserMessage("user question"),
					llm.AssistantToolCallMessage(llm.ToolUseBlock{Type: "tool_use", ID: "seed1", Name: "probe", Input: json.RawMessage(`{}`)}),
					{Role: llm.RoleTool, Content: []llm.ContentBlock{
						llm.ToolResultBlock{Type: "tool_result", ToolUseID: "seed1", Content: "out",
							Images: []llm.ImageBlock{{Type: "image", Data: []byte("IMGDATA")}}},
					}},
				}
			},
			mutate: func(h []llm.Message) {
				tr := h[3].Content[0].(llm.ToolResultBlock)
				tr.Images[0].Data[0] = 'Z'
			},
			verify: func(t *testing.T, label string, h []llm.Message) {
				tr := h[3].Content[0].(llm.ToolResultBlock)
				if len(tr.Images) != 1 || len(tr.Images[0].Data) == 0 || tr.Images[0].Data[0] != 'I' {
					t.Errorf("%s: tool result image data = %#v, want untouched", label, tr.Images)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			seed := tc.seed()
			p, result := runIsolationRounds(t, seed, tc.mutate)

			if len(p.snapshots) != 2 {
				t.Fatalf("provider calls = %d, want 2", len(p.snapshots))
			}
			tc.verify(t, "original", seed)
			tc.verify(t, "round-2 handover", p.snapshots[1])
			tc.verify(t, "done history", result.History)
		})
	}
}

func retentionSeed() []llm.Message {
	return []llm.Message{
		{Role: llm.RoleSystem, Content: []llm.ContentBlock{&llm.TextBlock{Type: "text", Text: "system policy"}}},
		llm.UserMessage("user question"),
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			llm.ReasoningItemBlock{Type: "reasoning_item", Summary: []string{"keep reasoning"}},
			&llm.ToolUseBlock{Type: "tool_use", ID: "seed1", Name: "probe", Input: json.RawMessage(`{"k":"v"}`)},
		}},
		{Role: llm.RoleTool, Content: []llm.ContentBlock{
			llm.ToolResultBlock{Type: "tool_result", ToolUseID: "seed1", Content: "out",
				Images: []llm.ImageBlock{{Type: "image", Data: []byte("IMGDATA")}}},
		}},
	}
}

func verifyRetentionSeed(t *testing.T, label string, h []llm.Message) {
	t.Helper()
	if got := h[0].Content[0].(*llm.TextBlock).Text; got != "system policy" {
		t.Errorf("%s: system text = %q, want %q", label, got, "system policy")
	}
	if got := h[1].Content[0].(llm.TextBlock).Text; got != "user question" {
		t.Errorf("%s: user text = %q, want %q", label, got, "user question")
	}
	ri := h[2].Content[0].(llm.ReasoningItemBlock)
	if len(ri.Summary) != 1 || ri.Summary[0] != "keep reasoning" {
		t.Errorf("%s: reasoning summary = %#v, want untouched", label, ri.Summary)
	}
	tu := h[2].Content[1].(*llm.ToolUseBlock)
	if len(tu.Input) == 0 || tu.Input[0] != '{' {
		t.Errorf("%s: tool use input = %q, want untouched", label, string(tu.Input))
	}
	tr := h[3].Content[0].(llm.ToolResultBlock)
	if len(tr.Images) != 1 || len(tr.Images[0].Data) == 0 || tr.Images[0].Data[0] != 'I' {
		t.Errorf("%s: tool result image = %#v, want untouched", label, tr.Images)
	}
}

func retentionMutation(h []llm.Message) {
	h[0].Content[0].(*llm.TextBlock).Text = "MUTATED-SYSTEM"
	h[1].Content[0] = llm.TextBlock{Type: "text", Text: "MUTATED-USER"}
	ri := h[2].Content[0].(llm.ReasoningItemBlock)
	ri.Summary[0] = "MUTATED-SUMMARY"
	tu := h[2].Content[1].(*llm.ToolUseBlock)
	tu.Input[0] = 'Z'
	tr := h[3].Content[0].(llm.ToolResultBlock)
	tr.Images[0].Data[0] = 'Z'
}

func TestProviderRetainedHistoryIsolation(t *testing.T) {
	t.Parallel()

	t.Run("sequenced mutation after return", func(t *testing.T) {
		t.Parallel()
		seed := retentionSeed()
		p := &scriptedProvider{steps: []scriptedStep{
			{stream: true, chunks: probeToolCallChunks()},
			{stream: true, chunks: probeTextChunks()},
		}}
		gated := &gatedTool{
			info:    tool.ToolInfo{Name: "probe", Description: "probe tool"},
			entered: make(chan struct{}),
			release: make(chan struct{}),
		}
		reg := tool.NewRegistry()
		reg.MustRegister(gated)
		ag := New(p, reg, WithLogger(discardLogger()))

		var runResult *RunResult
		runDone := make(chan error, 1)
		go func() {
			r, err := ag.RunWithHistory(context.Background(), seed, "go")
			runResult = r
			runDone <- err
		}()

		<-gated.entered
		retentionMutation(p.refs[0])
		close(gated.release)

		if err := <-runDone; err != nil {
			t.Fatalf("RunWithHistory() error = %v", err)
		}
		if len(p.snapshots) != 2 {
			t.Fatalf("provider calls = %d, want 2", len(p.snapshots))
		}
		if got := p.refs[0][0].Content[0].(*llm.TextBlock).Text; got != "MUTATED-SYSTEM" {
			t.Fatalf("retained round-1 copy was not mutated by the provider: %q", got)
		}
		verifyRetentionSeed(t, "original", seed)
		verifyRetentionSeed(t, "round-2 handover", p.snapshots[1])
		verifyRetentionSeed(t, "done history", runResult.History)
	})

	t.Run("concurrent mutation of retained request", func(t *testing.T) {
		t.Parallel()
		seed := retentionSeed()
		p := &scriptedProvider{steps: []scriptedStep{
			{stream: true, chunks: probeToolCallChunks()},
			{stream: true, chunks: probeTextChunks()},
		}}
		gated := &gatedTool{
			info:    tool.ToolInfo{Name: "probe", Description: "probe tool"},
			entered: make(chan struct{}),
			release: make(chan struct{}),
		}
		reg := tool.NewRegistry()
		reg.MustRegister(gated)
		ag := New(p, reg, WithLogger(discardLogger()))

		var runResult *RunResult
		runDone := make(chan error, 1)
		go func() {
			r, err := ag.RunWithHistory(context.Background(), seed, "go")
			runResult = r
			runDone <- err
		}()

		<-gated.entered
		var wg sync.WaitGroup
		wg.Add(1)
		retained := p.refs[0]
		go func() {
			defer wg.Done()
			retentionMutation(retained)
		}()
		close(gated.release)

		if err := <-runDone; err != nil {
			t.Fatalf("RunWithHistory() error = %v", err)
		}
		wg.Wait()
		verifyRetentionSeed(t, "original", seed)
		verifyRetentionSeed(t, "round-2 handover", p.snapshots[1])
		verifyRetentionSeed(t, "done history", runResult.History)
	})
}

func TestRetryDelaySingleResolution(t *testing.T) {
	t.Parallel()
	retryErr := &llm.APIError{StatusCode: 429, RetryAfter: 40 * time.Millisecond}
	p := &scriptedProvider{steps: []scriptedStep{
		{stream: true, err: retryErr},
		{stream: true, chunks: probeTextChunks()},
	}}
	var onRetryDelay time.Duration
	ag := newIsolationAgent(p, 3, WithRetryConfig(AgentRetryConfig{
		MaxRetries: 3,
		OnRetry:    func(info RetryInfo) { onRetryDelay = info.Delay },
	}))
	result, err := ag.RunWithHistory(context.Background(), retentionSeed(), "go")
	if err != nil {
		t.Fatalf("RunWithHistory() error = %v", err)
	}
	if len(p.snapshots) != 2 {
		t.Fatalf("provider calls = %d, want 2 (one failure + success)", len(p.snapshots))
	}
	if len(result.Retries) != 1 {
		t.Fatalf("retries = %d, want 1", len(result.Retries))
	}
	// Single resolution (issue #16): the announced delay IS the waited
	// delay — the Retry-After value, not an independent jitter draw.
	if got := result.Retries[0].Delay; got != 40*time.Millisecond {
		t.Errorf("RetryInfo.Delay = %v, want exactly the 40ms Retry-After", got)
	}
	if onRetryDelay != 40*time.Millisecond {
		t.Errorf("OnRetry delay = %v, want exactly the 40ms Retry-After", onRetryDelay)
	}
}

func TestSynthesizeChunksPointerForms(t *testing.T) {
	t.Parallel()
	usage := &llm.Usage{}
	blocks := []llm.ContentBlock{
		&llm.ReasoningBlock{Type: "reasoning", Content: "thinking"},
		(*llm.TextBlock)(nil), // typed nil: skipped, no chunk
		&llm.TextBlock{Type: "text", Text: "answer"},
		&llm.ToolUseBlock{ID: "n1", Name: "probe", Input: []byte("{}")},
		(*llm.ToolUseBlock)(nil), // typed nil: skipped
		&llm.ReasoningItemBlock{Type: "reasoning_item", ID: "r1"},
	}
	chunks := synthesizeChunks(blocks, usage)
	want := []llm.Chunk{
		llm.ReasoningDeltaChunk{Text: "thinking"},
		llm.TextDeltaChunk{Text: "answer", OutputIndex: 2},
		llm.ToolCallStartChunk{Index: 3, ID: "n1", Name: "probe"},
		llm.ToolCallArgsChunk{Index: 3, Delta: "{}"},
		llm.ReasoningItemChunk{OutputIndex: 5, Item: llm.ReasoningItemBlock{Type: "reasoning_item", ID: "r1"}},
		llm.DoneChunk{FinishReason: "tool_calls", Usage: usage},
	}
	if len(chunks) != len(want) {
		t.Fatalf("chunks = %#v (%d), want %d", chunks, len(chunks), len(want))
	}
	for i := range want {
		if !reflect.DeepEqual(chunks[i], want[i]) {
			t.Errorf("chunk[%d] = %#v, want %#v", i, chunks[i], want[i])
		}
	}
}

func TestMaxRetryConfigClamped(t *testing.T) {
	t.Parallel()
	ag := newIsolationAgent(&scriptedProvider{}, math.MaxInt)
	maxRetries, _, _ := ag.resolveRetryConfig()
	if maxRetries != maxRetryCap {
		t.Fatalf("resolveRetryConfig() maxRetries = %d, want clamped to maxRetryCap %d", maxRetries, maxRetryCap)
	}
	if maxRetries+1 <= 0 {
		t.Fatalf("maxAttempts = %d, want positive (no overflow)", maxRetries+1)
	}

	// End to end: MaxInt config must run the retry loop (not skip it and
	// return a nil stream), with the backoff arithmetic saturated.
	p := &scriptedProvider{steps: []scriptedStep{
		{stream: true, err: &llm.APIError{StatusCode: 500, Body: "boom"}},
		{stream: true, chunks: probeTextChunks()},
	}}
	ag2 := newIsolationAgent(p, math.MaxInt)
	result, err := ag2.RunWithHistory(context.Background(), retentionSeed(), "go")
	if err != nil {
		t.Fatalf("RunWithHistory() error = %v (nil-stream panic path)", err)
	}
	if len(p.snapshots) != 2 {
		t.Errorf("provider calls = %d, want 2 (retry loop ran)", len(p.snapshots))
	}
	if len(result.Retries) != 1 {
		t.Errorf("retries = %d, want 1", len(result.Retries))
	}
}

func TestProviderRetryHistoryIsolation(t *testing.T) {
	t.Parallel()
	seed := retentionSeed()
	retryErr := &llm.APIError{StatusCode: 500, Body: "boom"}
	p := &scriptedProvider{steps: []scriptedStep{
		{stream: true, chunks: probeToolCallChunks(), mutate: retentionMutation, err: retryErr},
		{stream: true, chunks: probeToolCallChunks(), mutate: retentionMutation, err: retryErr},
		{stream: true, chunks: probeTextChunks()},
	}}
	ag := newIsolationAgent(p, 2)
	result, err := ag.RunWithHistory(context.Background(), seed, "go")
	if err != nil {
		t.Fatalf("RunWithHistory() error = %v", err)
	}
	if len(p.snapshots) != 3 {
		t.Fatalf("provider calls = %d, want 3 (two failures + success)", len(p.snapshots))
	}
	if len(result.Retries) != 2 {
		t.Errorf("retries = %d, want 2", len(result.Retries))
	}
	verifyRetentionSeed(t, "original", seed)
	verifyRetentionSeed(t, "final handover", p.snapshots[2])
	verifyRetentionSeed(t, "done history", result.History)
}

func TestProviderFallbackHistoryIsolation(t *testing.T) {
	t.Parallel()

	t.Run("chat fallback receives clean copy", func(t *testing.T) {
		t.Parallel()
		seed := retentionSeed()
		msg := llm.AssistantMessage("fallback ok")
		p := &scriptedProvider{steps: []scriptedStep{
			{stream: true, mutate: retentionMutation, err: llm.ErrStreamingNotSupported},
			{stream: false, msg: &msg, mutate: retentionMutation},
		}}
		ag := newIsolationAgent(p, 0)
		result, err := ag.RunWithHistory(context.Background(), seed, "go")
		if err != nil {
			t.Fatalf("RunWithHistory() error = %v", err)
		}
		if len(p.snapshots) != 2 {
			t.Fatalf("provider calls = %d, want 2 (stream then chat)", len(p.snapshots))
		}
		verifyRetentionSeed(t, "original", seed)
		verifyRetentionSeed(t, "chat handover", p.snapshots[1])
		verifyRetentionSeed(t, "done history", result.History)
	})

	t.Run("retryable chat error re-enters stream with clean copy", func(t *testing.T) {
		t.Parallel()
		seed := retentionSeed()
		retryErr := &llm.APIError{StatusCode: 500, Body: "boom"}
		p := &scriptedProvider{steps: []scriptedStep{
			{stream: true, err: llm.ErrStreamingNotSupported},
			{stream: false, mutate: retentionMutation, err: retryErr},
			{stream: true, chunks: probeTextChunks()},
		}}
		ag := newIsolationAgent(p, 1)
		result, err := ag.RunWithHistory(context.Background(), seed, "go")
		if err != nil {
			t.Fatalf("RunWithHistory() error = %v", err)
		}
		if len(p.snapshots) != 3 {
			t.Fatalf("provider calls = %d, want 3 (stream, chat, retry stream)", len(p.snapshots))
		}
		verifyRetentionSeed(t, "original", seed)
		verifyRetentionSeed(t, "retry stream handover", p.snapshots[2])
		verifyRetentionSeed(t, "done history", result.History)
	})
}

func TestProviderRoundContextHistoryIsolation(t *testing.T) {
	t.Parallel()

	t.Run("no callback", func(t *testing.T) {
		t.Parallel()
		seed := retentionSeed()
		p, runResult := runIsolationRounds(t, seed, retentionMutation)
		verifyRetentionSeed(t, "original", seed)
		verifyRetentionSeed(t, "round-2 handover", p.snapshots[1])
		verifyRetentionSeed(t, "done history", runResult.History)
	})

	t.Run("empty snapshot", func(t *testing.T) {
		t.Parallel()
		seed := retentionSeed()
		p := &scriptedProvider{steps: []scriptedStep{
			{stream: true, chunks: probeToolCallChunks(), mutate: retentionMutation},
			{stream: true, chunks: probeTextChunks(), mutate: retentionMutation},
		}}
		ag := newIsolationAgent(p, 0, WithRoundContextProvider(func(_ context.Context, _ RoundContextRequest) (string, error) {
			return "", nil
		}))
		result, err := ag.RunWithHistory(context.Background(), seed, "go")
		if err != nil {
			t.Fatalf("RunWithHistory() error = %v", err)
		}
		verifyRetentionSeed(t, "original", seed)
		verifyRetentionSeed(t, "round-2 handover", p.snapshots[1])
		verifyRetentionSeed(t, "done history", result.History)
	})

	t.Run("snapshot applied fresh on every attempt", func(t *testing.T) {
		t.Parallel()
		seed := retentionSeed()
		p := &scriptedProvider{steps: []scriptedStep{
			{stream: true, chunks: probeToolCallChunks(), mutate: retentionMutation},
			{stream: true, chunks: probeTextChunks(), mutate: retentionMutation},
		}}
		ag := newIsolationAgent(p, 0, WithRoundContextProvider(func(_ context.Context, _ RoundContextRequest) (string, error) {
			return "ctx-snapshot-value", nil
		}))
		result, err := ag.RunWithHistory(context.Background(), seed, "go")
		if err != nil {
			t.Fatalf("RunWithHistory() error = %v", err)
		}
		for i, snap := range p.snapshots {
			blob, err := json.Marshal(snap)
			if err != nil {
				t.Fatalf("marshal snapshot %d: %v", i, err)
			}
			if !strings.Contains(string(blob), "ctx-snapshot-value") {
				t.Errorf("snapshot %d misses the round-context overlay", i)
			}
		}
		blob, err := json.Marshal(result.History)
		if err != nil {
			t.Fatalf("marshal history: %v", err)
		}
		if strings.Contains(string(blob), "ctx-snapshot-value") {
			t.Errorf("round-context overlay leaked into history")
		}
		verifyRetentionSeed(t, "original", seed)
		verifyRetentionSeed(t, "done history", result.History)
	})

	t.Run("oversized snapshot truncates without exposing history", func(t *testing.T) {
		t.Parallel()
		seed := retentionSeed()
		p := &scriptedProvider{steps: []scriptedStep{
			{stream: true, chunks: probeToolCallChunks(), mutate: retentionMutation},
			{stream: true, chunks: probeTextChunks(), mutate: retentionMutation},
		}}
		ag := newIsolationAgent(p, 0,
			WithContextWindowTokens(1000),
			WithRoundContextProvider(func(_ context.Context, _ RoundContextRequest) (string, error) {
				return strings.Repeat("x", 5000), nil
			}))
		result, err := ag.RunWithHistory(context.Background(), seed, "go")
		if err != nil {
			t.Fatalf("RunWithHistory() error = %v", err)
		}
		blob, err := json.Marshal(p.snapshots[0])
		if err != nil {
			t.Fatalf("marshal snapshot: %v", err)
		}
		if !strings.Contains(string(blob), "runtime context truncated") {
			t.Errorf("oversized snapshot was not truncated")
		}
		verifyRetentionSeed(t, "original", seed)
		verifyRetentionSeed(t, "round-2 handover", p.snapshots[1])
		verifyRetentionSeed(t, "done history", result.History)
	})
}

func TestProviderMaxIterHistoryIsolation(t *testing.T) {
	t.Parallel()
	seed := retentionSeed()
	gated := &gatedTool{info: tool.ToolInfo{Name: "probe", Description: "probe tool"}}
	p := &scriptedProvider{steps: []scriptedStep{
		{stream: true, chunks: probeToolCallChunks(), mutate: retentionMutation},
	}}
	reg := tool.NewRegistry()
	reg.MustRegister(gated)
	ag := New(p, reg, WithMaxIter(1), WithLogger(discardLogger()))
	result, err := ag.RunWithHistory(context.Background(), seed, "go")
	if err != nil {
		t.Fatalf("RunWithHistory() error = %v", err)
	}
	if len(p.snapshots) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(p.snapshots))
	}
	if !result.Truncated {
		t.Errorf("Truncated = false, want true for a maxIter terminal round")
	}
	if gated.executes != 0 {
		t.Errorf("tool executions = %d, want 0 on the maxIter terminal round", gated.executes)
	}
	verifyRetentionSeed(t, "original", seed)
	verifyRetentionSeed(t, "done history", result.History)
}

func TestProviderForeignBlockCompatibility(t *testing.T) {
	t.Parallel()
	seed := []llm.Message{
		llm.SystemMessage("system policy"),
		{Role: llm.RoleUser, Content: []llm.ContentBlock{
			wrappedTextBlock{&llm.TextBlock{Type: "text", Text: "wrapped payload"}},
		}},
	}
	p := &scriptedProvider{steps: []scriptedStep{
		{stream: true, chunks: probeTextChunks()},
	}}
	ag := newIsolationAgent(p, 0)
	result, err := ag.RunWithHistory(context.Background(), seed, "go")
	if err != nil {
		t.Fatalf("RunWithHistory() error = %v (foreign blocks must keep flowing)", err)
	}
	if len(p.snapshots) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(p.snapshots))
	}

	recv := p.refs[0]
	origWrapped, okOrig := seed[1].Content[0].(wrappedTextBlock)
	recvWrapped, okRecv := recv[1].Content[0].(wrappedTextBlock)
	if !okOrig || !okRecv {
		t.Fatalf("foreign block missing: seed=%v received=%v", okOrig, okRecv)
	}
	// D0 carve-out, pinned: the kernel does not clone foreign internals.
	if origWrapped.TextBlock != recvWrapped.TextBlock {
		t.Errorf("foreign block pointee was cloned; want shared reference through to the provider")
	}

	recv[1].Content[0] = llm.TextBlock{Type: "text", Text: "replaced"}
	if _, ok := seed[1].Content[0].(wrappedTextBlock); !ok {
		t.Errorf("original Content slice shares backing: wrapped block replaced")
	}
	recv[1] = llm.UserMessage("replaced")
	if _, ok := seed[1].Content[0].(wrappedTextBlock); !ok {
		t.Errorf("original outer slice shares backing: wrapped message replaced")
	}

	// Serial write through the shared pointee is visible to the original —
	// the documented residue of the carve-out.
	recvWrapped.Text = "MUTATED-BY-PROVIDER"
	if got := seed[1].Content[0].(wrappedTextBlock).Text; got != "MUTATED-BY-PROVIDER" {
		t.Errorf("foreign pointee write not shared with original: %q", got)
	}
	if result == nil {
		t.Errorf("RunResult = nil")
	}
}
