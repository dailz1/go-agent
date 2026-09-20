// Command agenttool composes a prebuilt child Agent into a parent registry with
// agenttool.New. Run `go run ./examples/agenttool`; it is deterministic and
// makes no network requests. The outer tool approval is independent from the
// child's registry and options. Each adapter Execute is a fresh child Run: it
// does not share parent threads, Store sessions, or child events.
package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/dailz1/go-agent/agent"
	"github.com/dailz1/go-agent/agenttest"
	"github.com/dailz1/go-agent/agenttool"
	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

func main() {
	result, err := run()
	if err != nil {
		panic(err)
	}
	fmt.Println(result.Message.Content[0].(llm.TextBlock).Text)
}

func run() (*agent.RunResult, error) {
	childProvider := agenttest.NewScriptedProvider(agenttest.Exchange{
		Method:       agenttest.MethodChatStream,
		Request:      agenttest.Request{Messages: []llm.Message{llm.UserMessage("summarize the task")}, Tools: []tool.ToolInfo{}},
		StreamChunks: []llm.Chunk{llm.TextDeltaChunk{Text: "child summary"}, llm.DoneChunk{FinishReason: "stop"}},
	})
	child := agent.New(childProvider, tool.NewRegistry(), agent.WithMaxIter(2)) // child has its own registry and limits
	delegated := agenttool.New(child, agenttool.Config{
		Name: "researcher", Description: "Summarizes a bounded research task.", RequiresApproval: true,
	})
	args := json.RawMessage(`{"input":"summarize the task"}`)
	parentProvider := agenttest.NewScriptedProvider(
		agenttest.Exchange{Method: agenttest.MethodChatStream, Request: agenttest.Request{Messages: []llm.Message{llm.UserMessage("delegate")}, Tools: []tool.ToolInfo{delegated.Info()}}, StreamChunks: []llm.Chunk{
			llm.ToolCallStartChunk{Index: 0, ID: "delegate-1", Name: "researcher"},
			llm.ToolCallArgsChunk{Index: 0, Delta: string(args)},
			llm.DoneChunk{FinishReason: "tool_calls"},
		}},
		agenttest.Exchange{Method: agenttest.MethodChatStream, Request: agenttest.Request{Messages: []llm.Message{llm.UserMessage("delegate"), llm.AssistantToolCallMessage(llm.ToolUseBlock{ID: "delegate-1", Name: "researcher", Input: args}), llm.ToolResultMessage("delegate-1", tool.NewTextResult("child summary"))}, Tools: []tool.ToolInfo{delegated.Info()}}, StreamChunks: []llm.Chunk{llm.TextDeltaChunk{Text: "parent final"}, llm.DoneChunk{FinishReason: "stop"}}},
	)
	registry := tool.NewRegistry()
	registry.MustRegister(delegated)
	result, err := agent.New(parentProvider, registry, agent.WithApprovalFn(func(tool.ToolInfo, json.RawMessage) bool { return true })).Run(context.Background(), "delegate")
	if err != nil {
		return nil, err
	}
	if err := childProvider.Verify(); err != nil {
		return nil, err
	}
	if err := parentProvider.Verify(); err != nil {
		return nil, err
	}
	return result, nil
}
