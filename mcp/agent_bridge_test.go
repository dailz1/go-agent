package mcpbridge

import (
	"context"
	"encoding/json"
	"iter"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/agent"
	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type oneToolProvider struct {
	calls    int
	messages [][]llm.Message
}

func (*oneToolProvider) Name() string { return "one-tool" }
func (p *oneToolProvider) Chat(_ context.Context, messages []llm.Message, _ []tool.ToolInfo, _ ...llm.Option) (*llm.Message, *llm.Usage, error) {
	p.messages = append(p.messages, messages)
	p.calls++
	if p.calls == 1 {
		message := llm.AssistantToolCallMessage(llm.ToolUseBlock{ID: "1", Name: "remote__large", Input: json.RawMessage(`{}`)})
		return &message, nil, nil
	}
	message := llm.AssistantMessage("done")
	return &message, nil, nil
}
func (*oneToolProvider) ChatStream(context.Context, []llm.Message, []tool.ToolInfo, ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	return nil, llm.ErrStreamingNotSupported
}

func TestAgentTruncatesContentButKeepsLiveData(t *testing.T) {
	payload := []byte("live")
	provider := &oneToolProvider{}
	_, registry := connectTestBridge(t, func(s *sdk.Server) {
		addTool(s, "large", func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return &sdk.CallToolResult{Content: []sdk.Content{
				&sdk.TextContent{Text: strings.Repeat("x", 600)},
				&sdk.ImageContent{MIMEType: "image/png", Data: payload},
			}}, nil
		})
	})
	sequence, err := agent.New(provider, registry, agent.WithContextWindowTokens(1000), agent.WithApprovalFn(func(tool.ToolInfo, json.RawMessage) bool { return true })).RunStream(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	var eventData json.RawMessage
	for event, err := range sequence {
		if err != nil {
			t.Fatal(err)
		}
		if result, ok := event.(agent.ToolResultEvent); ok {
			eventData = result.Result.Data
		}
	}
	wantData := `{"mcp_content":[{"index":1,"type":"image","mimeType":"image/png","data":"bGl2ZQ=="}]}`
	if string(eventData) != wantData {
		t.Fatalf("event Data=%s", eventData)
	}
	if len(provider.messages) != 2 {
		t.Fatalf("provider calls=%d", len(provider.messages))
	}
	last := provider.messages[1][len(provider.messages[1])-1].Content[0].(llm.ToolResultBlock).Content
	if !strings.Contains(last, "[tool result truncated:") || len([]rune(last)) != 300 {
		t.Fatalf("history content was not truncated: %q", last)
	}
}
