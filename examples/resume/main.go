// Command resume shows the minimal persistent-thread recovery flow without a
// network provider. Run it with `go run ./examples/resume`. Production callers
// can replace MemoryStore with store.NewJSONL and must retain their thread IDs.
// This example continues an incomplete RunThread with ResumeThread. Explicit
// SettleThread instead abandons that run and unlocks new input.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"

	"github.com/dailz1/go-agent/agent"
	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

const threadID = "resume-demo"

func main() {
	result, err := recoverThread()
	if err != nil {
		panic(err)
	}
	if result.Cancelled {
		fmt.Println("thread was explicitly cancelled; no model reply")
		return
	}
	fmt.Println(result.Message.Content[0].(llm.TextBlock).Text)
}

func recoverThread() (*agent.RunResult, error) {
	st := store.NewMemory()
	provider := &resumeProvider{}
	cancelled, cancel := context.WithCancel(context.Background())
	registry := tool.NewRegistry()
	registry.MustRegister(interruptTool{cancel: cancel})
	a := agent.New(provider, registry, agent.WithStore(st))
	_, err := a.RunThread(cancelled, threadID, "start work")
	if !errors.Is(err, context.Canceled) {
		return nil, fmt.Errorf("interrupt RunThread: %w", err)
	}
	return a.ResumeThread(context.Background(), threadID)
}

type interruptTool struct{ cancel context.CancelFunc }

func (t interruptTool) Info() tool.ToolInfo {
	return tool.ToolInfo{Name: "interrupt", Description: "ends the demo run", Parameters: tool.NewParameterSchema()}
}
func (t interruptTool) Execute(context.Context, json.RawMessage) (*tool.ToolResult, error) {
	t.cancel()
	return nil, context.Canceled
}

type resumeProvider struct{ round int }

func (*resumeProvider) Name() string { return "resume-demo" }
func (p *resumeProvider) Chat(context.Context, []llm.Message, []tool.ToolInfo, ...llm.Option) (*llm.Message, *llm.Usage, error) {
	p.round++
	if p.round == 1 {
		return &llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.ToolUseBlock{Type: "tool_use", ID: "interrupt", Name: "interrupt", Input: json.RawMessage(`{}`)}}}, nil, nil
	}
	return &llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock{Type: "text", Text: "recovered persistent thread"}}}, nil, nil
}
func (*resumeProvider) ChatStream(context.Context, []llm.Message, []tool.ToolInfo, ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	return nil, llm.ErrStreamingNotSupported
}
