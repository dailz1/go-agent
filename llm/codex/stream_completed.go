package codex

import (
	"slices"

	"github.com/dailz1/go-agent/llm"
)

func (s *streamState) complete(outputs []outputItem) ([]llm.Chunk, string, error) {
	var chunks []llm.Chunk
	previous := -1
	for position, output := range outputs {
		index := position
		if known, ok := s.ids[output.ID]; ok {
			index = known
		}
		if index <= previous {
			return nil, "", protocolError("duplicate or reordered completed item")
		}
		previous = index
		if item := s.items[index]; item != nil {
			var err error
			output, err = item.completeOutput(output)
			if err != nil {
				return nil, "", err
			}
		}
		part, err := s.finishItem(index, output)
		if err != nil {
			return nil, "", err
		}
		chunks = append(chunks, part...)
	}
	finish := "stop"
	indices := make([]int, 0, len(s.items))
	for index := range s.items {
		indices = append(indices, index)
	}
	slices.Sort(indices)
	for _, index := range indices {
		item := s.items[index]
		if !item.done {
			output, err := item.completeOutput(outputItem{ID: item.item.ID})
			if err != nil {
				return nil, "", err
			}
			part, err := s.finishItem(index, output)
			if err != nil {
				return nil, "", err
			}
			chunks = append(chunks, part...)
		}
		if item.item.Type == "function_call" {
			finish = "tool_calls"
		}
	}
	return chunks, finish, nil
}

// completeOutput fills only missing terminal fields. Explicit values still pass
// through finishItem's identity, content, argument, and size checks.
func (item *itemState) completeOutput(output outputItem) (outputItem, error) {
	if output.Type == "" {
		output.Type = item.item.Type
	}
	switch output.Type {
	case "message":
		if output.Role == "" {
			output.Role = "assistant"
		}
		if len(output.Content) == 0 {
			output.Content = item.item.Content
		}
		for index := range max(len(output.Content), len(item.parts)) {
			part := item.parts[index]
			if index >= len(output.Content) {
				if part == nil {
					return outputItem{}, protocolError("completed message has a content index gap")
				}
				output.Content = append(output.Content, contentPart{})
			}
			if part == nil {
				continue
			}
			content := &output.Content[index]
			if content.Type == "" {
				content.Type = part.kind
			}
			text := part.text.String()
			if content.Type == "output_text" && content.Text == nil {
				content.Text = &text
			}
			if content.Type == "refusal" && content.Refusal == nil {
				content.Refusal = &text
			}
		}
	case "function_call":
		if output.CallID == "" {
			output.CallID = item.item.CallID
		}
		if output.Name == "" {
			output.Name = item.item.Name
		}
		if output.Arguments == nil {
			args := item.args.String()
			output.Arguments = &args
		}
	case "reasoning":
		if output.EncryptedContent == "" {
			output.EncryptedContent = item.item.EncryptedContent
		}
		if len(output.Summary) == 0 {
			output.Summary = item.item.Summary
		}
		for index := len(output.Summary); index < max(len(item.item.Summary), len(item.summaries)); index++ {
			if index < len(item.item.Summary) {
				output.Summary = append(output.Summary, item.item.Summary[index])
				continue
			}
			part := item.summaries[index]
			if part == nil {
				return outputItem{}, protocolError("completed reasoning has a summary index gap")
			}
			output.Summary = append(output.Summary, summaryPart{Type: "summary_text", Text: part.text.String()})
		}
	}
	return output, nil
}
