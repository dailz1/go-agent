// Command testdouble shows deterministic Agent tests with agenttest. Run
// `go run ./examples/testdouble`; it makes no network requests. ScriptedProvider
// verifies the exact provider exchange and ToolFunc records the tool arguments.
package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/dailz1/go-agent/agent"
	"github.com/dailz1/go-agent/agenttest"
	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

func main() {
	result, calls, err := run()
	if err != nil {
		panic(err)
	}
	fmt.Printf("%s (tool calls: %d)\n", result.Message.Content[0].(llm.TextBlock).Text, len(calls))
}

func run() (*agent.RunResult, []agenttest.ToolCall, error) {
	echo := &agenttest.ToolFunc{
		Definition: echoInfo(),
		ExecuteFunc: func(_ context.Context, args json.RawMessage) (*tool.ToolResult, error) {
			var input struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(args, &input); err != nil {
				return tool.NewErrorResult("invalid arguments: %v", err), nil
			}
			return tool.NewTextResult(input.Text), nil
		},
	}
	args := json.RawMessage(`{"text":"hello from a deterministic tool"}`)
	initialRequest := agenttest.Request{Messages: []llm.Message{llm.UserMessage("demonstrate a tool")}, Tools: []tool.ToolInfo{echoInfo()}}
	finalRequest := agenttest.Request{Messages: []llm.Message{llm.UserMessage("demonstrate a tool"), llm.AssistantToolCallMessage(llm.ToolUseBlock{ID: "echo-1", Name: "echo", Input: args}), llm.ToolResultMessage("echo-1", tool.NewTextResult("hello from a deterministic tool"))}, Tools: []tool.ToolInfo{echoInfo()}}
	provider := agenttest.NewScriptedProvider(
		agenttest.Exchange{Method: agenttest.MethodChatStream, Request: initialRequest, StreamOuterErr: llm.ErrStreamingNotSupported},
		agenttest.Exchange{Method: agenttest.MethodChat, Request: initialRequest, ChatResponse: ptr(llm.AssistantToolCallMessage(llm.ToolUseBlock{ID: "echo-1", Name: "echo", Input: args}))},
		agenttest.Exchange{Method: agenttest.MethodChatStream, Request: finalRequest, StreamOuterErr: llm.ErrStreamingNotSupported},
		agenttest.Exchange{Method: agenttest.MethodChat, Request: finalRequest, ChatResponse: ptr(llm.AssistantMessage("script verified"))},
	)
	registry := tool.NewRegistry()
	registry.MustRegister(echo)
	result, err := agent.New(provider, registry).Run(context.Background(), "demonstrate a tool")
	if err != nil {
		return nil, nil, err
	}
	if err := provider.Verify(); err != nil {
		return nil, nil, err
	}
	return result, echo.Calls(), nil
}

func ptr(message llm.Message) *llm.Message { return &message }

func echoInfo() tool.ToolInfo {
	schema := tool.NewParameterSchema()
	schema.Properties["text"] = tool.Param("string", "text to echo")
	schema.Required = []string{"text"}
	return tool.ToolInfo{Name: "echo", Description: "returns text", Parameters: schema}
}
