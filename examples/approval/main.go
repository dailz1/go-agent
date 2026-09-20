// Command approval demonstrates deterministic tool approval behavior without a
// network provider. Run it with `go run ./examples/approval`. Tools marked
// RequiresApproval are rejected when no callback is configured; callbacks must
// make the allow/reject decision and tools are not a sandbox.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"

	"github.com/dailz1/go-agent/agent"
	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

func main() {
	for _, allow := range []bool{false, true} {
		result, err := run(allow)
		if err != nil {
			panic(err)
		}
		fmt.Printf("approval=%v: %s\n", allow, result.Message.Content[0].(llm.TextBlock).Text)
	}
}

func run(allow bool) (*agent.RunResult, error) {
	registry := tool.NewRegistry()
	registry.MustRegister(noteTool{})
	return agent.New(&approvalProvider{}, registry, agent.WithApprovalFn(func(tool.ToolInfo, json.RawMessage) bool {
		return allow
	})).Run(context.Background(), "show the approval result")
}

type noteTool struct{}

func (noteTool) Info() tool.ToolInfo {
	schema := tool.NewParameterSchema()
	return tool.ToolInfo{Name: "write_note", Description: "writes a demo note", Parameters: schema, RequiresApproval: true}
}
func (noteTool) Execute(context.Context, json.RawMessage) (*tool.ToolResult, error) {
	return tool.NewTextResult("note written"), nil
}

type approvalProvider struct{ round int }

func (*approvalProvider) Name() string { return "approval-demo" }
func (p *approvalProvider) Chat(context.Context, []llm.Message, []tool.ToolInfo, ...llm.Option) (*llm.Message, *llm.Usage, error) {
	p.round++
	if p.round == 1 {
		return &llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.ToolUseBlock{Type: "tool_use", ID: "write", Name: "write_note", Input: json.RawMessage(`{}`)}}}, nil, nil
	}
	return &llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock{Type: "text", Text: "approval handled"}}}, nil, nil
}
func (p *approvalProvider) ChatStream(context.Context, []llm.Message, []tool.ToolInfo, ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	return nil, llm.ErrStreamingNotSupported
}
