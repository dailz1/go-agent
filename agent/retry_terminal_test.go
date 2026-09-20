package agent

import (
	"context"
	"errors"
	"iter"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

// countingFailProvider always returns a retryable 429 from ChatStream and
// counts the calls, so the terminal-retry path can be pinned by signal and
// counter: no sleeps, no polling.
type countingFailProvider struct {
	calls   int
	chatted int
}

func (p *countingFailProvider) Name() string { return "counting_fail" }

func (p *countingFailProvider) Chat(_ context.Context, _ []llm.Message, _ []tool.ToolInfo, _ ...llm.Option) (*llm.Message, *llm.Usage, error) {
	p.chatted++
	return nil, nil, llm.ErrStreamingNotSupported
}

func (p *countingFailProvider) ChatStream(_ context.Context, _ []llm.Message, _ []tool.ToolInfo, _ ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	p.calls++
	return nil, &llm.APIError{StatusCode: 429, Body: "rate limited"}
}

// TestTerminalRetryExhaustion pins the terminal-retry path: when every
// attempt (first try plus all retries) fails with a retryable error, the run
// surfaces exactly one terminal error, the OnRetry callback fires exactly
// MaxRetries times, and the provider receives no further call after the last
// attempt.
func TestTerminalRetryExhaustion(t *testing.T) {
	provider := &countingFailProvider{}
	retries := 0
	agent := New(provider, tool.NewRegistry(), WithLogger(discardLogger()),
		WithRetryConfig(AgentRetryConfig{MaxRetries: 3, BaseDelay: 1, MaxDelay: 1,
			OnRetry: func(info RetryInfo) { retries++ }}))

	stream, err := agent.RunStream(context.Background(), "hi")
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	var terminal []error
	for _, err := range stream {
		if err != nil {
			terminal = append(terminal, err)
		}
	}

	if len(terminal) != 1 {
		t.Fatalf("terminal errors = %d, want exactly 1: %v", len(terminal), terminal)
	}
	if retries != 3 {
		t.Errorf("OnRetry calls = %d, want 3", retries)
	}
	if provider.calls != 4 {
		t.Errorf("provider ChatStream calls = %d, want 4 (first try + 3 retries)", provider.calls)
	}
	// No non-streaming fallback was taken either: ChatStream errors are
	// retryable API errors, not ErrStreamingNotSupported.
	if provider.chatted != 0 {
		t.Errorf("provider Chat calls = %d, want 0", provider.chatted)
	}
	var apiErr *llm.APIError
	if !errors.As(terminal[0], &apiErr) || apiErr.StatusCode != 429 {
		t.Errorf("terminal error = %v, want wrapped *llm.APIError 429", terminal[0])
	}
	if !strings.Contains(terminal[0].Error(), "after 4 attempt(s)") {
		t.Errorf("terminal error %q should report exhaustion after 4 attempts", terminal[0])
	}
}
