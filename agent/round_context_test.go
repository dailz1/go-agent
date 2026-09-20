package agent

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"strings"
	"testing"
	"time"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

type capturedProviderCall struct {
	stream  bool
	history []llm.Message
}

type capturedStreamResult struct {
	chunks []llm.Chunk
	err    error
}

type capturedChatResult struct {
	message *llm.Message
	err     error
}

type capturedProvider struct {
	streams      []capturedStreamResult
	chats        []capturedChatResult
	calls        []capturedProviderCall
	onChatStream func()
}

func (p *capturedProvider) Name() string { return "captured" }

func (p *capturedProvider) ChatStream(_ context.Context, history []llm.Message, _ []tool.ToolInfo, _ ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	p.calls = append(p.calls, capturedProviderCall{stream: true, history: deepCopyMessages(history)})
	if p.onChatStream != nil {
		p.onChatStream()
	}
	if len(p.streams) == 0 {
		return nil, errors.New("unexpected ChatStream call")
	}
	result := p.streams[0]
	p.streams = p.streams[1:]
	if result.err != nil {
		return nil, result.err
	}
	return func(yield func(llm.Chunk, error) bool) {
		for _, chunk := range result.chunks {
			if !yield(chunk, nil) {
				return
			}
		}
	}, nil
}

func (p *capturedProvider) Chat(_ context.Context, history []llm.Message, _ []tool.ToolInfo, _ ...llm.Option) (*llm.Message, *llm.Usage, error) {
	p.calls = append(p.calls, capturedProviderCall{history: deepCopyMessages(history)})
	if len(p.chats) == 0 {
		return nil, nil, errors.New("unexpected Chat call")
	}
	result := p.chats[0]
	p.chats = p.chats[1:]
	return result.message, nil, result.err
}

func capturedFinalText(call capturedProviderCall) string {
	return messageText(&call.history[len(call.history)-1])
}

func TestRoundContextProviderCore(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{
			name: "nil provider changes no request",
			run: func(t *testing.T) {
				p := &capturedProvider{streams: []capturedStreamResult{{chunks: []llm.Chunk{
					llm.TextDeltaChunk{Text: "done"}, llm.DoneChunk{FinishReason: "stop"},
				}}}}
				ag := New(p, tool.NewRegistry(), WithRoundContextProvider(nil), WithLogger(discardLogger()))
				if _, err := ag.Run(context.Background(), "input"); err != nil {
					t.Fatalf("Run() error = %v", err)
				}
				if got := capturedFinalText(p.calls[0]); got != "input" {
					t.Errorf("outbound final text = %q, want input without overlay", got)
				}
			},
		},
		{
			name: "empty snapshot sends no overlay",
			run: func(t *testing.T) {
				p := &capturedProvider{streams: []capturedStreamResult{{chunks: []llm.Chunk{llm.DoneChunk{FinishReason: "stop"}}}}}
				calls := 0
				ag := New(p, tool.NewRegistry(), WithRoundContextProvider(func(context.Context, RoundContextRequest) (string, error) {
					calls++
					return "", nil
				}), WithLogger(discardLogger()))
				if _, err := ag.Run(context.Background(), "input"); err != nil {
					t.Fatalf("Run() error = %v", err)
				}
				if calls != 1 || capturedFinalText(p.calls[0]) != "input" {
					t.Errorf("calls, outbound = %d, %q; want 1, input", calls, capturedFinalText(p.calls[0]))
				}
			},
		},
		{
			name: "callback error stops before provider",
			run: func(t *testing.T) {
				want := errors.New("snapshot unavailable")
				p := &capturedProvider{}
				ag := New(p, tool.NewRegistry(), WithRoundContextProvider(func(context.Context, RoundContextRequest) (string, error) {
					return "", want
				}), WithLogger(discardLogger()))
				_, err := ag.Run(context.Background(), "input")
				var contextErr *RoundContextError
				if !errors.As(err, &contextErr) || !errors.Is(err, want) {
					t.Fatalf("Run() error = %v, want RoundContextError wrapping %v", err, want)
				}
				if contextErr.Round != 0 || contextErr.Attempt != 1 || len(p.calls) != 0 {
					t.Errorf("round, attempt, provider calls = %d, %d, %d; want 0, 1, 0", contextErr.Round, contextErr.Attempt, len(p.calls))
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

func TestRoundContextProviderOverlayBudgetBoundaries(t *testing.T) {
	history := []llm.Message{llm.UserMessage("input")}
	exactWindow := 2_000
	exactCap := exactWindow / 5
	exactSnapshot := ""
	for runes := 0; runes <= exactCap; runes++ {
		candidate := strings.Repeat("x", runes)
		if estimateOverlayIncrement(history, roundContextMessage(candidate)) == exactCap {
			exactSnapshot = candidate
			break
		}
	}
	if exactSnapshot == "" {
		t.Fatalf("could not construct an overlay exactly at the %d-rune cap", exactCap)
	}
	t.Run("overlay exactly at cap passes", func(t *testing.T) {
		p := &capturedProvider{streams: []capturedStreamResult{{chunks: []llm.Chunk{
			llm.DoneChunk{FinishReason: "stop"},
		}}}}
		ag := New(p, tool.NewRegistry(),
			WithContextWindowTokens(exactWindow),
			WithRoundContextProvider(func(context.Context, RoundContextRequest) (string, error) {
				return exactSnapshot, nil
			}),
			WithLogger(discardLogger()),
		)
		if _, err := ag.Run(context.Background(), "input"); err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		if got := estimateOverlayIncrement(history, p.calls[0].history[len(p.calls[0].history)-1]); got != exactCap {
			t.Errorf("outbound overlay increment = %d, want exact cap %d", got, exactCap)
		}
	})

	overWindow := 0
	minimalTruncated := "x" + roundContextTruncationMarker + "x"
	for window := 1; window < 3_000; window++ {
		cap := window / 5
		if cap+1 == estimateOverlayIncrement(history, roundContextMessage(minimalTruncated)) &&
			estimateOverlayIncrement(history, roundContextMessage("")) <= cap {
			overWindow = window
			break
		}
	}
	if overWindow == 0 {
		t.Fatal("could not construct a cap one rune below the smallest truncated overlay")
	}
	t.Run("one rune over the truncation cap fails", func(t *testing.T) {
		p := &capturedProvider{}
		callbacks := 0
		ag := New(p, tool.NewRegistry(),
			WithContextWindowTokens(overWindow),
			WithRoundContextProvider(func(context.Context, RoundContextRequest) (string, error) {
				callbacks++
				return strings.Repeat("x", 10_000), nil
			}),
			WithLogger(discardLogger()),
		)
		_, err := ag.Run(context.Background(), "input")
		if !errors.Is(err, ErrCompactionBudgetExceeded) || callbacks != 1 || len(p.calls) != 0 {
			t.Errorf("Run() error, callback calls, provider calls = %v, %d, %d; want budget error, 1, 0", err, callbacks, len(p.calls))
		}
	})
}

func TestRoundContextProviderEveryRoundAndAttempt(t *testing.T) {
	registry := tool.NewRegistry()
	registry.MustRegister(&mockTool{info: tool.ToolInfo{Name: "echo"}, result: tool.NewTextResult("ok")})
	p := &capturedProvider{streams: []capturedStreamResult{
		{chunks: []llm.Chunk{
			llm.ToolCallStartChunk{Index: 0, ID: "call", Name: "echo"},
			llm.ToolCallArgsChunk{Index: 0, Delta: `{}`},
			llm.DoneChunk{FinishReason: "tool_calls"},
		}},
		{chunks: []llm.Chunk{llm.DoneChunk{FinishReason: "stop"}}},
	}}
	var requests []RoundContextRequest
	ag := New(p, registry, WithRoundContextProvider(func(_ context.Context, request RoundContextRequest) (string, error) {
		requests = append(requests, request)
		return "steady-snapshot", nil
	}), WithLogger(discardLogger()))
	result, err := ag.Run(context.Background(), "input")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(requests) != 2 || requests[0].Round != 0 || requests[1].Round != 1 ||
		requests[0].Attempt != 1 || requests[1].Attempt != 1 {
		t.Fatalf("requests = %#v, want rounds 0/1 and attempts 1/1", requests)
	}
	for index, call := range p.calls {
		if !strings.Contains(capturedFinalText(call), "steady-snapshot") {
			t.Errorf("request %d missing unchanged snapshot: %q", index, capturedFinalText(call))
		}
	}
	for _, message := range result.History {
		if strings.Contains(messageText(&message), "steady-snapshot") {
			t.Fatalf("Done history contains transient snapshot: %#v", message)
		}
	}
}

func TestRoundContextProviderStaysOutOfPreDoneFoldArtifacts(t *testing.T) {
	p := &capturedProvider{streams: []capturedStreamResult{{chunks: []llm.Chunk{
		llm.TextDeltaChunk{Text: "model response"}, llm.DoneChunk{FinishReason: "stop"},
	}}}}
	ag := New(p, tool.NewRegistry(), WithRoundContextProvider(func(context.Context, RoundContextRequest) (string, error) {
		return "fold-isolation-snapshot", nil
	}), WithLogger(discardLogger()))
	seq, err := ag.RunStream(context.Background(), "input")
	if err != nil {
		t.Fatalf("RunStream() error = %v", err)
	}
	fold := newEventFold([]llm.Message{llm.UserMessage("input")})
	for event, iterErr := range seq {
		if iterErr != nil {
			t.Fatalf("RunStream event error = %v", iterErr)
		}
		if done, ok := event.(DoneEvent); ok {
			for _, message := range done.History {
				if strings.Contains(messageText(&message), "fold-isolation-snapshot") {
					t.Fatal("DoneEvent.History retained the outbound overlay")
				}
			}
			continue
		}
		fold.apply(t, event)
		for _, message := range fold.full() {
			if strings.Contains(messageText(&message), "fold-isolation-snapshot") {
				t.Fatal("pre-Done fold artifact retained the outbound overlay")
			}
		}
	}
	if !strings.Contains(capturedFinalText(p.calls[0]), "fold-isolation-snapshot") {
		t.Fatal("provider request did not receive the overlay")
	}
}

func TestRoundContextProviderCancellationBeforeFallbackSkipsCallback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := &capturedProvider{
		streams: []capturedStreamResult{{err: llm.ErrStreamingNotSupported}},
		onChatStream: func() {
			cancel()
		},
	}
	callbacks := 0
	ag := New(p, tool.NewRegistry(),
		WithRoundContextProvider(func(context.Context, RoundContextRequest) (string, error) {
			callbacks++
			return "snapshot", nil
		}),
		WithLogger(discardLogger()),
	)
	_, err := ag.Run(ctx, "input")
	var contextErr *RoundContextError
	if callbacks != 1 || len(p.calls) != 1 || !errors.Is(err, context.Canceled) || errors.As(err, &contextErr) {
		t.Fatalf("callbacks, provider calls, error, RoundContextError = %d, %d, %v, %v; want 1, 1, canceled, false",
			callbacks, len(p.calls), err, contextErr)
	}
}

func TestRoundContextProviderTruncationSurvivesOversizedToolResult(t *testing.T) {
	oversizedToolResult := "tool-first" + strings.Repeat("x", 10_000) + "tool-last"
	oversizedSnapshot := "overlay-first" + strings.Repeat("中", 10_000) + "overlay-last"
	registry := tool.NewRegistry()
	registry.MustRegister(&mockTool{
		info:   tool.ToolInfo{Name: "echo"},
		result: tool.NewTextResult(oversizedToolResult),
	})
	p := &capturedProvider{streams: []capturedStreamResult{
		{chunks: []llm.Chunk{
			llm.ToolCallStartChunk{Index: 0, ID: "call", Name: "echo"},
			llm.ToolCallArgsChunk{Index: 0, Delta: `{}`},
			llm.DoneChunk{FinishReason: "tool_calls"},
		}},
		{chunks: []llm.Chunk{llm.DoneChunk{FinishReason: "stop"}}},
	}}
	ag := New(p, registry,
		WithContextWindowTokens(4_000),
		WithRoundContextProvider(func(context.Context, RoundContextRequest) (string, error) {
			return oversizedSnapshot, nil
		}),
		WithLogger(discardLogger()),
	)
	if _, err := ag.Run(context.Background(), "input"); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(p.calls) != 2 {
		t.Fatalf("provider calls = %d, want 2", len(p.calls))
	}
	for index, call := range p.calls {
		overlay := capturedFinalText(call)
		if !strings.Contains(overlay, "overlay-first") || !strings.Contains(overlay, "overlay-last") ||
			len([]rune(overlay)) >= len([]rune(oversizedSnapshot)) {
			t.Errorf("call %d overlay = %q; want successfully truncated snapshot retaining both boundaries", index, overlay)
		}
	}
	var outboundToolResult string
	for _, message := range p.calls[1].history {
		for _, block := range message.Content {
			if result, ok := block.(llm.ToolResultBlock); ok {
				outboundToolResult = result.Content
			}
		}
	}
	if !strings.Contains(outboundToolResult, "tool-first") || !strings.Contains(outboundToolResult, "tool-last") ||
		len([]rune(outboundToolResult)) >= len([]rune(oversizedToolResult)) {
		t.Errorf("outbound tool result = %q; want independently truncated result retaining both boundaries", outboundToolResult)
	}
}

func TestRoundContextProviderRetryAndFallbackAttempts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		provider *capturedProvider
		want     []int
	}{
		{
			name: "stream fallback increments",
			provider: &capturedProvider{
				streams: []capturedStreamResult{{err: llm.ErrStreamingNotSupported}},
				chats:   []capturedChatResult{{message: &llm.Message{Role: llm.RoleAssistant}}},
			},
			want: []int{1, 2},
		},
		{
			name: "direct retry increments",
			provider: &capturedProvider{streams: []capturedStreamResult{
				{err: &llm.APIError{StatusCode: 500, Body: "retry"}},
				{chunks: []llm.Chunk{llm.DoneChunk{FinishReason: "stop"}}},
			}},
			want: []int{1, 2},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var attempts []int
			ag := New(tt.provider, tool.NewRegistry(),
				WithRetryConfig(AgentRetryConfig{MaxRetries: 1, BaseDelay: time.Nanosecond}),
				WithRoundContextProvider(func(_ context.Context, request RoundContextRequest) (string, error) {
					attempts = append(attempts, request.Attempt)
					return fmt.Sprintf("attempt-%d", request.Attempt), nil
				}),
				WithLogger(discardLogger()),
			)
			if _, err := ag.Run(context.Background(), "input"); err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if fmt.Sprint(attempts) != fmt.Sprint(tt.want) {
				t.Errorf("attempts = %v, want %v", attempts, tt.want)
			}
			if got := capturedFinalText(tt.provider.calls[len(tt.provider.calls)-1]); !strings.Contains(got, fmt.Sprintf("attempt-%d", tt.want[len(tt.want)-1])) {
				t.Errorf("final outbound request = %q, want latest snapshot", got)
			}
		})
	}
}

func TestRoundContextProviderFallbackRetryAttemptDivergence(t *testing.T) {
	p := &capturedProvider{
		streams: []capturedStreamResult{
			{err: llm.ErrStreamingNotSupported},
			{chunks: []llm.Chunk{llm.DoneChunk{FinishReason: "stop"}}},
		},
		chats: []capturedChatResult{{err: &llm.APIError{StatusCode: 500, Body: "retry"}}},
	}
	var attempts []int
	ag := New(p, tool.NewRegistry(),
		WithRetryConfig(AgentRetryConfig{MaxRetries: 1, BaseDelay: time.Nanosecond}),
		WithRoundContextProvider(func(_ context.Context, request RoundContextRequest) (string, error) {
			attempts = append(attempts, request.Attempt)
			return fmt.Sprintf("attempt-%d", request.Attempt), nil
		}),
		WithLogger(discardLogger()),
	)
	result, err := ag.Run(context.Background(), "input")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got, want := fmt.Sprint(attempts), "[1 2 3]"; got != want {
		t.Errorf("direct provider attempts = %s, want %s", got, want)
	}
	if len(result.Retries) != 1 || result.Retries[0].Attempt != 2 {
		t.Errorf("RetryInfo = %#v, want one retry with attempt 2", result.Retries)
	}
}
