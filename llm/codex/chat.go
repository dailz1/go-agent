package codex

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

// Chat folds the same reconciled SSE stream as ChatStream, in output-item order.
func (p *Provider) Chat(ctx context.Context, messages []llm.Message, tools []tool.ToolInfo, options ...llm.Option) (*llm.Message, *llm.Usage, error) {
	seq, err := p.ChatStream(ctx, messages, tools, options...)
	if err != nil {
		return nil, nil, err
	}
	type foldItem struct {
		text      strings.Builder
		args      strings.Builder
		call      *llm.ToolUseBlock
		reasoning *llm.ReasoningItemBlock
	}
	items := make(map[int]*foldItem)
	get := func(index int) *foldItem {
		if items[index] == nil {
			items[index] = &foldItem{}
		}
		return items[index]
	}
	var usage *llm.Usage
	for chunk, err := range seq {
		if err != nil {
			return nil, nil, err
		}
		switch c := chunk.(type) {
		case llm.TextDeltaChunk:
			get(int(c.OutputIndex)).text.WriteString(c.Text)
		case llm.ToolCallStartChunk:
			get(c.Index).call = &llm.ToolUseBlock{Type: "tool_use", ID: c.ID, Name: c.Name}
		case llm.ToolCallArgsChunk:
			get(c.Index).args.WriteString(c.Delta)
		case llm.ReasoningItemChunk:
			get(int(c.OutputIndex)).reasoning = &c.Item
		case llm.ReasoningDeltaChunk:
			// Summaries are carried by the complete reasoning item.
		case llm.DoneChunk:
			usage = c.Usage
		}
	}
	indices := make([]int, 0, len(items))
	for index := range items {
		indices = append(indices, index)
	}
	slices.Sort(indices)
	message := &llm.Message{Role: llm.RoleAssistant, Content: make([]llm.ContentBlock, 0, len(items))}
	for _, index := range indices {
		item := items[index]
		switch {
		case item.reasoning != nil:
			message.Content = append(message.Content, *item.reasoning)
		case item.call != nil:
			item.call.Input = json.RawMessage(item.args.String())
			message.Content = append(message.Content, *item.call)
		default:
			message.Content = append(message.Content, llm.TextBlock{Type: "text", Text: item.text.String()})
		}
	}
	return message, usage, nil
}
