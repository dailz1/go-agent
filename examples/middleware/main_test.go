package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/pkg/agent"
	"github.com/dailz1/go-agent/pkg/llm"
	"github.com/dailz1/go-agent/pkg/tool"
)

// noStreamProvider fails streaming with the sentinel and serves Chat.
type noStreamProvider struct {
	chatCalled bool
}

func (p *noStreamProvider) Name() string { return "no_stream" }

func (p *noStreamProvider) Chat(ctx context.Context, messages []llm.Message, tools []tool.ToolInfo, opts ...llm.Option) (*llm.Message, *llm.Usage, error) {
	p.chatCalled = true
	return &llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock{Type: "text", Text: "fallback-ok"}}}, nil, nil
}

func (p *noStreamProvider) ChatStream(ctx context.Context, messages []llm.Message, tools []tool.ToolInfo, opts ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	return nil, llm.ErrStreamingNotSupported
}

// signalProvider's stream notifies cleanup when its iterator stops
// (naturally or via early break).
type signalProvider struct {
	cleanup chan struct{}
}

func (p *signalProvider) Name() string { return "signal" }

func (p *signalProvider) Chat(ctx context.Context, messages []llm.Message, tools []tool.ToolInfo, opts ...llm.Option) (*llm.Message, *llm.Usage, error) {
	return nil, nil, errors.New("unexpected Chat")
}

func (p *signalProvider) ChatStream(ctx context.Context, messages []llm.Message, tools []tool.ToolInfo, opts ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	return func(yield func(llm.Chunk, error) bool) {
		defer close(p.cleanup)
		for _, c := range []llm.Chunk{llm.TextDeltaChunk{Text: "a"}, llm.TextDeltaChunk{Text: "b"}} {
			if !yield(c, nil) {
				return
			}
		}
	}, nil
}

type fixedTool struct {
	info tool.ToolInfo
}

func (t *fixedTool) Info() tool.ToolInfo { return t.info }

func (t *fixedTool) Execute(ctx context.Context, args json.RawMessage) (*tool.ToolResult, error) {
	return tool.NewTextResult("SECRET-PAYLOAD-CONTENT"), nil
}

func testSchema() tool.ParameterSchema {
	s := tool.NewParameterSchema()
	s.Properties["q"] = tool.Param("string", "query")
	s.Required = []string{"q"}
	return s
}

// TestLoggingProviderPreservesStreamingNotSupported: the wrapped provider's
// outer error keeps errors.Is(llm.ErrStreamingNotSupported) and the agent
// falls back to Chat successfully.
func TestLoggingProviderPreservesStreamingNotSupported(t *testing.T) {
	inner := &noStreamProvider{}
	wrapped := loggingProvider{Provider: inner, log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	_, err := wrapped.ChatStream(context.Background(), nil, nil)
	if !errors.Is(err, llm.ErrStreamingNotSupported) {
		t.Fatalf("errors.Is(ErrStreamingNotSupported) = false, err = %v", err)
	}

	a := agent.New(wrapped, tool.NewRegistry())
	res, err := a.Run(context.Background(), "hi")
	if err != nil {
		t.Fatalf("agent Run: %v", err)
	}
	if !inner.chatCalled {
		t.Fatal("agent did not fall back to Chat")
	}
	if got := res.Message.Content; len(got) != 1 || got[0].(llm.TextBlock).Text != "fallback-ok" {
		t.Fatalf("unexpected fallback message: %+v", res.Message.Content)
	}
}

// TestLoggingToolPassesFullToolInfo: schema and RequiresApproval survive the
// wrapper unchanged.
func TestLoggingToolPassesFullToolInfo(t *testing.T) {
	inner := &fixedTool{info: tool.ToolInfo{
		Name:             "search",
		Description:      "search things",
		Parameters:       testSchema(),
		RequiresApproval: true,
	}}
	wrapped := loggingTool{Tool: inner, log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	got := wrapped.Info()
	if got.Name != inner.info.Name || got.Description != inner.info.Description || got.RequiresApproval != inner.info.RequiresApproval {
		t.Fatalf("info mismatch: %+v", got)
	}
	if fmt.Sprint(got.Parameters) != fmt.Sprint(inner.info.Parameters) {
		t.Fatalf("schema mismatch: %+v vs %+v", got.Parameters, inner.info.Parameters)
	}

	ctxKey := new(int)
	ctx := context.WithValue(context.Background(), ctxKey, "v") //nolint:staticcheck — identity check only
	res, err := wrapped.Execute(ctx, json.RawMessage(`{"q":"x"}`))
	if err != nil || res == nil || res.IsError() {
		t.Fatalf("Execute: res=%v err=%v", res, err)
	}
}

// TestRateLimitStreamEarlyBreakReleasesPermit: breaking after the first chunk
// releases the permit and notifies the inner iterator's cleanup.
func TestRateLimitStreamEarlyBreakReleasesPermit(t *testing.T) {
	inner := &signalProvider{cleanup: make(chan struct{})}
	wrapped := NewRateLimitStreamProvider(inner, 1)

	seq, err := wrapped.ChatStream(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}

	chunks := 0
	for range seq {
		chunks++
		break // early exit after the first chunk
	}
	if chunks != 1 {
		t.Fatalf("chunks = %d, want 1", chunks)
	}

	// Inner cleanup has been notified (closed channel, no sleep/poll).
	select {
	case <-inner.cleanup:
	default:
		t.Fatal("inner cleanup not notified after early break")
	}
	// Permit released.
	select {
	case wrapped.sem <- struct{}{}:
		<-wrapped.sem
	default:
		t.Fatal("rate-limit permit leaked after early break")
	}
}

// TestLoggingToolDoesNotCopyPayload: logs carry metadata only — the full
// tool result text must not appear in the log output.
func TestLoggingToolDoesNotCopyPayload(t *testing.T) {
	var buf bytes.Buffer
	wrapped := loggingTool{Tool: &fixedTool{info: tool.ToolInfo{Name: "search"}}, log: slog.New(slog.NewTextHandler(&buf, nil))}

	payload := strings.Repeat("SECRET-PAYLOAD-CONTENT", 20)
	res, err := wrapped.Execute(context.Background(), json.RawMessage(`{"q":"x"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	_ = res
	_ = payload

	out := buf.String()
	if !strings.Contains(out, "search") {
		t.Fatalf("log missing tool name metadata: %q", out)
	}
	if strings.Contains(out, "SECRET-PAYLOAD-CONTENT") {
		t.Fatalf("log copied payload text: %q", out)
	}
}
