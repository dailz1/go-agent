package mcpbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/agent"
	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type progressProvider struct {
	calls    int
	messages [][]llm.Message
}

func (*progressProvider) Name() string { return "progress" }
func (p *progressProvider) Chat(_ context.Context, messages []llm.Message, _ []tool.ToolInfo, _ ...llm.Option) (*llm.Message, *llm.Usage, error) {
	p.messages = append(p.messages, messages)
	p.calls++
	if p.calls == 1 {
		message := llm.AssistantToolCallMessage(llm.ToolUseBlock{ID: "1", Name: "remote__progress", Input: json.RawMessage(`{}`)})
		return &message, nil, nil
	}
	message := llm.AssistantMessage("done")
	return &message, nil, nil
}
func (*progressProvider) ChatStream(context.Context, []llm.Message, []tool.ToolInfo, ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	return nil, llm.ErrStreamingNotSupported
}

func TestProgressNotificationIsNotAnAgentEvent(t *testing.T) {
	server := sdk.NewServer(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	var session *sdk.ServerSession
	server.AddTool(&sdk.Tool{Name: "progress", InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, _ *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		if err := session.NotifyProgress(ctx, &sdk.ProgressNotificationParams{ProgressToken: "sentinel", Progress: 1, Total: 2, Message: "progress-hidden"}); err != nil {
			return nil, err
		}
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "ok"}}}, nil
	})
	st, ct := sdk.NewInMemoryTransports()
	var err error
	session, err = server.Connect(context.Background(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry()
	bridge, err := Connect(context.Background(), registry, ct, Config{Namespace: "remote"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = bridge.Close() }()
	provider := &progressProvider{}
	sequence, err := agent.New(provider, registry, agent.WithApprovalFn(func(tool.ToolInfo, json.RawMessage) bool { return true })).RunStream(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	var events []string
	var result *tool.ToolResult
	for event, err := range sequence {
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, fmt.Sprintf("%#v", event))
		if emitted, ok := event.(agent.ToolResultEvent); ok {
			result = emitted.Result
		}
	}
	if result == nil || result.Content != "ok" || result.Data != nil {
		t.Fatalf("tool result=%+v", result)
	}
	if len(provider.messages) != 2 {
		t.Fatalf("provider calls=%d", len(provider.messages))
	}
	for _, output := range append(events, fmt.Sprintf("%#v", provider.messages)) {
		if strings.Contains(output, "progress-hidden") {
			t.Fatalf("progress leaked: %s", output)
		}
	}
}
