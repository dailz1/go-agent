// Command toolsearch demonstrates deferred tool loading: the model starts
// with only the tool_search entry, searches the deferred pool, and the next
// round advertises the loaded tools with full schemas. The model turns are
// replayed by a minimal recording provider, so it is deterministic and offline.
//
// Run it with:
//
//	go run ./examples/toolsearch
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"

	"github.com/dailz1/go-agent/agent"
	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
	"github.com/dailz1/go-agent/toolsearch"
)

type stubTool struct {
	info tool.ToolInfo
}

func (t *stubTool) Info() tool.ToolInfo { return t.info }

func (t *stubTool) Execute(context.Context, json.RawMessage) (*tool.ToolResult, error) {
	return tool.NewTextResult("ran " + t.info.Name), nil
}

func advertised(provider *recordingProvider, round int) []string {
	names := make([]string, 0, len(provider.calls[round].tools))
	for _, info := range provider.calls[round].tools {
		names = append(names, info.Name)
	}
	return names
}

type recordingProvider struct {
	chunks [][]llm.Chunk
	calls  []recordedCall
	next   int
}

type recordedCall struct {
	tools    []tool.ToolInfo
	messages []llm.Message
}

func (p *recordingProvider) Name() string { return "toolsearch_example" }

func (p *recordingProvider) Chat(context.Context, []llm.Message, []tool.ToolInfo, ...llm.Option) (*llm.Message, *llm.Usage, error) {
	return nil, nil, fmt.Errorf("chat path not scripted")
}

func (p *recordingProvider) ChatStream(_ context.Context, messages []llm.Message, tools []tool.ToolInfo, _ ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	i := p.next
	p.next++
	p.calls = append(p.calls, recordedCall{tools: tools, messages: messages})
	if i >= len(p.chunks) {
		return nil, fmt.Errorf("no scripted round remaining")
	}
	chunks := p.chunks[i]
	return func(yield func(llm.Chunk, error) bool) {
		for _, c := range chunks {
			if !yield(c, nil) {
				return
			}
		}
	}, nil
}

func main() {
	source := tool.NewRegistry()
	for _, t := range []tool.Tool{
		&stubTool{info: tool.ToolInfo{Name: "pdf_fill", Description: "Fill PDF forms", Parameters: tool.NewParameterSchema()}},
		&stubTool{info: tool.ToolInfo{Name: "pdf_flatten", Description: "Flatten PDF form fields", Parameters: tool.NewParameterSchema()}},
		&stubTool{info: tool.ToolInfo{Name: "weather", Description: "Weather lookup", Parameters: tool.NewParameterSchema()}},
	} {
		source.MustRegister(t)
	}
	exposed := tool.NewRegistry()
	if _, err := toolsearch.Connect(context.Background(), source, exposed); err != nil {
		panic(err)
	}

	searchArgs, _ := json.Marshal(map[string]string{"query": "pdf form"})
	provider := &recordingProvider{chunks: [][]llm.Chunk{
		{
			&llm.ToolCallStartChunk{Index: 0, ID: "call-1", Name: "tool_search"},
			&llm.ToolCallArgsChunk{Index: 0, ID: "call-1", Delta: string(searchArgs)},
			&llm.DoneChunk{},
		},
		{&llm.TextDeltaChunk{Text: "Found two PDF tools; they are advertised now."}, &llm.DoneChunk{}},
	}}
	a := agent.New(provider, exposed, agent.WithDynamicTools())
	if _, err := a.Run(context.Background(), "work with pdf forms"); err != nil {
		panic(err)
	}

	fmt.Println("round 1 advertised:", advertised(provider, 0))
	fmt.Println("round 2 advertised:", advertised(provider, 1))
}
