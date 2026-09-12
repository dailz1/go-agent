//go:build e2e
// +build e2e

package openairesponses

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/dailz1/go-agent/pkg/llm"
	"github.com/dailz1/go-agent/pkg/tool"
)

// Real-API integration test for the Responses adapter. Requires
// OPENAI_API_KEY; OPENAI_MODEL (default "gpt-4o-mini", use a Responses-capable
// model) and OPENAI_BASE_URL are optional. Run with:
//
//	go test -tags e2e ./pkg/llm/openairesponses/
//
// Reasoning-item replay requires a reasoning-capable model — set OPENAI_MODEL
// accordingly (e.g. gpt-5-mini); non-reasoning models accept the replayed
// items but gain nothing.
func TestE2EChatAndStream(t *testing.T) {
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		t.Skip("OPENAI_API_KEY not set")
	}
	model := os.Getenv("OPENAI_MODEL")
	if model == "" {
		model = "gpt-4o-mini"
	}
	p := NewProvider(apiKey, model, providerOptionsFromEnv()...)
	ctx := context.Background()

	// Non-streaming Chat with a tool call round trip.
	registry := tool.NewRegistry()
	registry.MustRegister(echoTool())

	chat := func(ctx context.Context, messages []llm.Message) (*llm.Message, *llm.Usage) {
		msg, usage, err := p.Chat(ctx, messages, registry.List())
		if err != nil {
			t.Fatalf("Chat: %v", err)
		}
		return msg, usage
	}

	first, _ := chat(ctx, []llm.Message{llm.UserMessage("Use the echo tool to echo the word hello.")})
	var called bool
	for _, blk := range first.Content {
		if _, ok := blk.(llm.ToolUseBlock); ok {
			called = true
		}
	}
	if !called {
		t.Fatalf("expected a tool call in the first response, got %+v", first)
	}

	// Full stateless replay: assistant items (incl. reasoning items) plus the
	// tool output must be accepted by the API.
	second := []llm.Message{
		llm.UserMessage("Use the echo tool to echo the word hello."),
		*first,
		llm.ToolResultMessage(toolCallID(first), tool.NewTextResult("hello")),
	}
	msg, _, err := p.Chat(ctx, second, registry.List())
	if err != nil {
		t.Fatalf("replay Chat: %v", err)
	}
	if msgText(msg) == "" {
		t.Error("replay response has no text")
	}

	// Streaming.
	seq, err := p.ChatStream(ctx, []llm.Message{llm.UserMessage("Say hi in one word.")}, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	sawDone := false
	var text string
	for c, err := range seq {
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		switch e := c.(type) {
		case llm.TextDeltaChunk:
			text += e.Text
		case llm.DoneChunk:
			sawDone = true
			if e.Usage == nil {
				t.Error("DoneChunk has no usage")
			}
		}
	}
	if !sawDone || text == "" {
		t.Errorf("stream incomplete: done=%v text=%q", sawDone, text)
	}
}

func providerOptionsFromEnv() []ProviderOption {
	var opts []ProviderOption
	if base := os.Getenv("OPENAI_BASE_URL"); base != "" {
		opts = append(opts, WithBaseURL(base))
	}
	return opts
}

func echoTool() tool.Tool {
	return echoToolT{}
}

type echoToolT struct{}

func (echoToolT) Info() tool.ToolInfo {
	return tool.ToolInfo{
		Name:        "echo",
		Description: "Echoes the given word back.",
		Parameters: tool.ParameterSchema{
			Type:       "object",
			Properties: map[string]tool.Property{"word": tool.Param("string", "word to echo")},
			Required:   []string{"word"},
		},
	}
}

func (echoToolT) Execute(_ context.Context, args json.RawMessage) (*tool.ToolResult, error) {
	var p struct {
		Word string `json:"word"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return tool.NewErrorResult("invalid arguments: %v", err), nil
	}
	return tool.NewTextResult(p.Word), nil
}

func toolCallID(m *llm.Message) string {
	for _, blk := range m.Content {
		if tb, ok := blk.(llm.ToolUseBlock); ok {
			return tb.ID
		}
	}
	return ""
}

func msgText(m *llm.Message) string {
	s := ""
	for _, blk := range m.Content {
		if tb, ok := blk.(llm.TextBlock); ok {
			s += tb.Text
		}
	}
	return s
}
