// Command parallel demonstrates WithToolConcurrency. Run it with
// `go run ./examples/parallel`. Tool results are committed in the model's
// declaration order even when execution overlaps; every real tool must honor
// its context cancellation.
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
	result, err := run(newDemoTool("one"), newDemoTool("two"))
	if err != nil {
		panic(err)
	}
	fmt.Println(result.Message.Content[0].(llm.TextBlock).Text)
}

func run(first, second tool.Tool) (*agent.RunResult, error) {
	registry := tool.NewRegistry()
	registry.MustRegister(first)
	registry.MustRegister(second)
	return agent.New(&parallelProvider{}, registry, agent.WithToolConcurrency(2)).Run(context.Background(), "run both")
}

type demoTool struct {
	name string
	run  func(context.Context)
}

func newDemoTool(name string) *demoTool { return &demoTool{name: name} }
func (t *demoTool) Info() tool.ToolInfo {
	return tool.ToolInfo{Name: t.name, Description: "parallel demo tool", Parameters: tool.NewParameterSchema()}
}
func (t *demoTool) Execute(ctx context.Context, _ json.RawMessage) (*tool.ToolResult, error) {
	if t.run != nil {
		t.run(ctx)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return tool.NewTextResult(t.name), nil
}

type parallelProvider struct{ round int }

func (*parallelProvider) Name() string { return "parallel-demo" }
func (p *parallelProvider) Chat(context.Context, []llm.Message, []tool.ToolInfo, ...llm.Option) (*llm.Message, *llm.Usage, error) {
	p.round++
	if p.round == 1 {
		return &llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			llm.ToolUseBlock{Type: "tool_use", ID: "first", Name: "one", Input: json.RawMessage(`{}`)},
			llm.ToolUseBlock{Type: "tool_use", ID: "second", Name: "two", Input: json.RawMessage(`{}`)},
		}}, nil, nil
	}
	return &llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock{Type: "text", Text: "parallel work complete"}}}, nil, nil
}
func (*parallelProvider) ChatStream(context.Context, []llm.Message, []tool.ToolInfo, ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	return nil, llm.ErrStreamingNotSupported
}
