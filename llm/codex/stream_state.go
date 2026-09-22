package codex

import (
	"encoding/json"
	"reflect"
	"strings"

	"github.com/dailz1/go-agent/llm"
)

type textState struct {
	kind      string
	text      strings.Builder
	delivered int
	done      bool
	deltaSeen bool
}

type itemState struct {
	item      outputItem
	parts     map[int]*textState
	summaries map[int]*textState
	args      strings.Builder
	argsDone  bool
	argsDelta bool
	started   bool
	done      bool
	size      int
}

type streamState struct {
	items map[int]*itemState
	ids   map[string]int
	calls map[string]int
}

func (s *streamState) item(index int, id, kind string) (*itemState, error) {
	if index < 0 || id == "" {
		return nil, protocolError("missing item index or id")
	}
	if prior, ok := s.ids[id]; ok && prior != index {
		return nil, protocolError("item id changed index")
	}
	item := s.items[index]
	if item == nil {
		item = &itemState{item: outputItem{ID: id, Type: kind}, parts: make(map[int]*textState), summaries: make(map[int]*textState)}
		s.items[index], s.ids[id] = item, index
	}
	if item.item.ID != id || item.item.Type != kind {
		return nil, protocolError("item identity changed")
	}
	return item, nil
}

func (s *streamState) register(index int, output outputItem) (*itemState, []llm.Chunk, error) {
	switch output.Type {
	case "message", "function_call", "reasoning":
	default:
		return nil, nil, protocolError("unsupported output item %q", output.Type)
	}
	item, err := s.item(index, output.ID, output.Type)
	if err != nil {
		return nil, nil, err
	}
	if output.Status != "" && output.Status != "in_progress" && output.Status != "completed" {
		return nil, nil, protocolError("invalid item status")
	}
	var chunks []llm.Chunk
	switch output.Type {
	case "message":
		if output.Role != "assistant" {
			return nil, nil, protocolError("invalid output message role")
		}
		for _, content := range output.Content {
			if _, err := contentText(content); err != nil {
				return nil, nil, err
			}
		}
		item.item.Role = output.Role
	case "reasoning":
		for _, summary := range output.Summary {
			if summary.Type != "summary_text" {
				return nil, nil, protocolError("unknown reasoning summary type")
			}
		}
	case "function_call":
		if output.CallID == "" || output.Name == "" {
			return nil, nil, protocolError("missing function identity")
		}
		if prior, ok := s.calls[output.CallID]; ok && prior != index {
			return nil, nil, protocolError("duplicate call id")
		}
		if item.started && (output.CallID != item.item.CallID || output.Name != item.item.Name) {
			return nil, nil, protocolError("function identity changed")
		}
		if !item.started {
			item.started = true
			item.item.CallID, item.item.Name = output.CallID, output.Name
			s.calls[output.CallID] = index
			chunks = append(chunks, llm.ToolCallStartChunk{Index: index, ID: output.CallID, Name: output.Name})
		}
	}
	return item, chunks, nil
}

func (item *itemState) appendText(part *textState, text string) error {
	if item.size > llm.MaxResponseBody-len(text) {
		return protocolError("item exceeds 10 MiB")
	}
	item.size += len(text)
	part.text.WriteString(text)
	return nil
}

func (item *itemState) text(index int, kind string) (*textState, error) {
	if index < 0 {
		return nil, protocolError("missing content index")
	}
	part := item.parts[index]
	if part == nil {
		part = &textState{kind: kind}
		item.parts[index] = part
	}
	if part.kind != kind {
		return nil, protocolError("content type changed")
	}
	return part, nil
}

func (item *itemState) finishText(part *textState, text string) error {
	if part.deltaSeen || part.done {
		if part.text.String() != text {
			return protocolError("completed text differs from deltas")
		}
	} else if err := item.appendText(part, text); err != nil {
		return err
	}
	part.done = true
	return nil
}

func (item *itemState) flushText(index int) []llm.Chunk {
	var chunks []llm.Chunk
	for i := 0; ; i++ {
		part := item.parts[i]
		if part == nil {
			break
		}
		if part.delivered < part.text.Len() {
			chunks = append(chunks, llm.TextDeltaChunk{OutputIndex: int64(index), Text: part.text.String()[part.delivered:]})
			part.delivered = part.text.Len()
		}
		if !part.done {
			break
		}
	}
	return chunks
}

func (item *itemState) finishArgs(index int, full *string) ([]llm.Chunk, error) {
	if full == nil || !json.Valid([]byte(*full)) {
		return nil, protocolError("invalid complete function arguments")
	}
	if item.argsDelta || item.argsDone {
		if item.args.String() != *full {
			return nil, protocolError("completed arguments differ from deltas")
		}
		item.argsDone = true
		return nil, nil
	}
	if item.size > llm.MaxResponseBody-len(*full) {
		return nil, protocolError("item exceeds 10 MiB")
	}
	item.size += len(*full)
	item.args.WriteString(*full)
	item.argsDone = true
	return []llm.Chunk{llm.ToolCallArgsChunk{Index: index, ID: item.item.CallID, Delta: *full}}, nil
}

func (s *streamState) finishItem(index int, output outputItem) ([]llm.Chunk, error) {
	item, chunks, err := s.register(index, output)
	if err != nil {
		return nil, err
	}
	if output.Status != "" && output.Status != "completed" {
		return nil, protocolError("unfinished output item")
	}
	if item.done {
		prior := item.item
		prior.Status, output.Status = "", ""
		if !reflect.DeepEqual(prior, output) {
			return nil, protocolError("completed item changed")
		}
		return nil, nil
	}
	switch output.Type {
	case "function_call":
		args, err := item.finishArgs(index, output.Arguments)
		if err != nil {
			return nil, err
		}
		chunks = append(chunks, args...)
	case "message":
		for partIndex, content := range output.Content {
			text, err := contentText(content)
			if err != nil {
				return nil, err
			}
			part, err := item.text(partIndex, content.Type)
			if err != nil {
				return nil, err
			}
			if err := item.finishText(part, text); err != nil {
				return nil, err
			}
		}
		if len(item.parts) != len(output.Content) {
			return nil, protocolError("completed message omitted content")
		}
		chunks = append(chunks, item.flushText(index)...)
	case "reasoning":
		block := llm.ReasoningItemBlock{Type: "reasoning_item", ID: output.ID, EncryptedContent: output.EncryptedContent}
		size := len(output.EncryptedContent)
		for i, summary := range output.Summary {
			if summary.Type != "summary_text" {
				return nil, protocolError("unknown reasoning summary type")
			}
			if part := item.summaries[i]; part != nil && (part.deltaSeen || part.done) && part.text.String() != summary.Text {
				return nil, protocolError("reasoning summary differs from deltas")
			}
			block.Summary = append(block.Summary, summary.Text)
			size += len(summary.Text)
		}
		for i := range item.summaries {
			if i >= len(output.Summary) {
				return nil, protocolError("missing reasoning summary")
			}
		}
		if size > llm.MaxResponseBody {
			return nil, protocolError("reasoning exceeds 10 MiB")
		}
		chunks = append(chunks, llm.ReasoningItemChunk{OutputIndex: int64(index), Item: block})
	}
	item.item, item.done = output, true
	return chunks, nil
}

func contentText(content contentPart) (string, error) {
	switch content.Type {
	case "output_text":
		if content.Text != nil {
			return *content.Text, nil
		}
	case "refusal":
		if content.Refusal != nil {
			return *content.Refusal, nil
		}
	}
	return "", protocolError("unsupported or missing content")
}
