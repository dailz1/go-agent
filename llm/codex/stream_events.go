package codex

import (
	"strings"

	"github.com/dailz1/go-agent/llm"
)

func (s *streamState) event(ev streamEvent) ([]llm.Chunk, bool, error) {
	switch ev.Type {
	case "response.completed":
		if ev.Response == nil || ev.Response.Status != "completed" || ev.Response.Error != nil {
			return nil, false, protocolError("invalid completed response")
		}
		chunks, finish, err := s.complete(ev.Response.Output)
		if err != nil {
			return nil, false, err
		}
		var usage *llm.Usage
		if u := ev.Response.Usage; u != nil {
			usage = &llm.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, ReasoningTokens: u.Details.ReasoningTokens}
		}
		return append(chunks, llm.DoneChunk{FinishReason: finish, Usage: usage}), true, nil
	case "response.failed", "response.incomplete", "error":
		server := ev.Error
		if ev.Response != nil && ev.Response.Error != nil {
			server = ev.Response.Error
		}
		if server == nil {
			server = &serverError{Code: ev.Code}
		}
		return nil, false, classifyServerError(server, nil)
	case "response.output_item.added", "response.output_item.done":
		if ev.OutputIndex == nil || ev.Item == nil {
			return nil, false, protocolError("missing output item or index")
		}
		if ev.Type == "response.output_item.done" {
			chunks, err := s.finishItem(*ev.OutputIndex, *ev.Item)
			return chunks, false, err
		}
		item, chunks, err := s.register(*ev.OutputIndex, *ev.Item)
		if err != nil {
			return nil, false, err
		}
		if item.done {
			return nil, false, protocolError("item added after completion")
		}
		return chunks, false, nil
	case "response.output_text.delta", "response.output_text.done", "response.refusal.delta", "response.refusal.done":
		chunks, err := s.textEvent(ev)
		return chunks, false, err
	case "response.function_call_arguments.delta", "response.function_call_arguments.done":
		chunks, err := s.argumentEvent(ev)
		return chunks, false, err
	case "response.reasoning_summary_text.delta", "response.reasoning_summary_text.done":
		chunks, err := s.summaryEvent(ev)
		return chunks, false, err
	case "response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
		if ev.Part == nil || ev.Part.Type != "summary_text" || ev.Part.Text == nil ||
			ev.OutputIndex == nil || ev.SummaryIndex == nil || *ev.SummaryIndex < 0 {
			return nil, false, protocolError("invalid reasoning summary part")
		}
		item, err := s.item(*ev.OutputIndex, ev.ItemID, "reasoning")
		if err != nil {
			return nil, false, err
		}
		if item.done {
			return nil, false, protocolError("summary after completed item")
		}
		if ev.Type == "response.reasoning_summary_part.added" {
			if item.summaries[*ev.SummaryIndex] == nil {
				item.summaries[*ev.SummaryIndex] = &textState{}
			}
			return nil, false, nil
		}
		ev.Type, ev.Text = "response.reasoning_summary_text.done", ev.Part.Text
		chunks, err := s.summaryEvent(ev)
		return chunks, false, err
	case "response.content_part.added", "response.content_part.done":
		if ev.Part == nil || ev.OutputIndex == nil || ev.ContentIndex == nil {
			return nil, false, protocolError("missing content part")
		}
		if _, err := contentText(*ev.Part); err != nil {
			return nil, false, err
		}
		item, err := s.item(*ev.OutputIndex, ev.ItemID, "message")
		if err != nil {
			return nil, false, err
		}
		if item.done {
			return nil, false, protocolError("content after completed item")
		}
		if ev.Type == "response.content_part.added" {
			_, err := item.text(*ev.ContentIndex, ev.Part.Type)
			return nil, false, err
		}
		ev.Type = "response." + ev.Part.Type + ".done"
		ev.Text, ev.Refusal = ev.Part.Text, ev.Part.Refusal
		chunks, err := s.textEvent(ev)
		return chunks, false, err
	default:
		// Lifecycle notifications carry no output; unknown content must not disappear.
		if ev.Item != nil || ev.Part != nil || ev.Delta != nil || ev.Text != nil || ev.Refusal != nil || ev.Arguments != nil ||
			strings.Contains(ev.Type, "_call.") || strings.Contains(ev.Type, "output_") {
			return nil, false, protocolError("unsupported content event %q", ev.Type)
		}
		if ev.Type == "" {
			return nil, false, protocolError("missing event type")
		}
		return nil, false, nil
	}
}

func (s *streamState) textEvent(ev streamEvent) ([]llm.Chunk, error) {
	if ev.OutputIndex == nil || ev.ContentIndex == nil {
		return nil, protocolError("missing text index")
	}
	item, err := s.item(*ev.OutputIndex, ev.ItemID, "message")
	if err != nil {
		return nil, err
	}
	if item.done {
		return nil, protocolError("text after completed item")
	}
	kind := "output_text"
	full := ev.Text
	if strings.HasPrefix(ev.Type, "response.refusal.") {
		kind, full = "refusal", ev.Refusal
	}
	part, err := item.text(*ev.ContentIndex, kind)
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(ev.Type, ".delta") {
		if ev.Delta == nil || part.done || item.done {
			return nil, protocolError("invalid text delta")
		}
		part.deltaSeen = true
		if err := item.appendText(part, *ev.Delta); err != nil {
			return nil, err
		}
	} else {
		if full == nil {
			return nil, protocolError("missing complete text")
		}
		if err := item.finishText(part, *full); err != nil {
			return nil, err
		}
	}
	return item.flushText(*ev.OutputIndex), nil
}

func (s *streamState) argumentEvent(ev streamEvent) ([]llm.Chunk, error) {
	if ev.OutputIndex == nil {
		return nil, protocolError("missing function index")
	}
	item := s.items[*ev.OutputIndex]
	if item == nil || !item.started || ev.ItemID == "" || item.item.ID != ev.ItemID {
		return nil, protocolError("arguments without matching start")
	}
	if ev.Type == "response.function_call_arguments.done" {
		return item.finishArgs(*ev.OutputIndex, ev.Arguments)
	}
	if ev.Delta == nil || item.argsDone || item.done {
		return nil, protocolError("arguments after completion")
	}
	if item.size > llm.MaxResponseBody-len(*ev.Delta) {
		return nil, protocolError("item exceeds 10 MiB")
	}
	item.size += len(*ev.Delta)
	item.argsDelta = true
	item.args.WriteString(*ev.Delta)
	return []llm.Chunk{llm.ToolCallArgsChunk{Index: *ev.OutputIndex, ID: item.item.CallID, Delta: *ev.Delta}}, nil
}

func (s *streamState) summaryEvent(ev streamEvent) ([]llm.Chunk, error) {
	if ev.OutputIndex == nil || ev.SummaryIndex == nil || *ev.SummaryIndex < 0 {
		return nil, protocolError("missing reasoning index")
	}
	item, err := s.item(*ev.OutputIndex, ev.ItemID, "reasoning")
	if err != nil {
		return nil, err
	}
	if item.done {
		return nil, protocolError("summary after completed item")
	}
	part := item.summaries[*ev.SummaryIndex]
	if part == nil {
		part = &textState{}
		item.summaries[*ev.SummaryIndex] = part
	}
	if ev.Type == "response.reasoning_summary_text.delta" {
		if ev.Delta == nil || item.done || part.done {
			return nil, protocolError("invalid summary delta")
		}
		part.deltaSeen = true
		if err := item.appendText(part, *ev.Delta); err != nil {
			return nil, err
		}
		part.delivered = part.text.Len()
		return []llm.Chunk{llm.ReasoningDeltaChunk{Text: *ev.Delta}}, nil
	}
	if ev.Text == nil {
		return nil, protocolError("missing complete summary")
	}
	if err := item.finishText(part, *ev.Text); err != nil {
		return nil, err
	}
	text := part.text.String()[part.delivered:]
	part.delivered = part.text.Len()
	if text == "" {
		return nil, nil
	}
	return []llm.Chunk{llm.ReasoningDeltaChunk{Text: text}}, nil
}
