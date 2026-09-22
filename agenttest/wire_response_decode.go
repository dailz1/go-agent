package agenttest

import (
	"encoding/json"

	"github.com/dailz1/go-agent/llm"
)

func decodeWireChat(raw json.RawMessage, version int) (*dtoChat, error) {
	object, err := wireObject(raw, "response", "usage", "error")
	if err != nil {
		return nil, err
	}
	response, err := wireRaw(object, "response")
	if err != nil {
		return nil, err
	}
	usage, err := wireRaw(object, "usage")
	if err != nil {
		return nil, err
	}
	recordedErr, err := wireRaw(object, "error")
	if err != nil {
		return nil, err
	}
	out := &dtoChat{}
	if string(response) != "null" {
		message, err := decodeWireMessage(response)
		if err != nil {
			return nil, err
		}
		out.Response = &message
	}
	if string(usage) != "null" {
		value, err := decodeWireUsage(usage)
		if err != nil {
			return nil, err
		}
		out.Usage = &value
	}
	if string(recordedErr) != "null" {
		out.Error, err = decodeWireError(recordedErr, version, "")
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
func decodeWireStream(raw json.RawMessage, version int) (*dtoStream, error) {
	object, err := wireObject(raw, "chunks", "outer_error", "stream_error", "completion")
	if err != nil {
		return nil, err
	}
	out := &dtoStream{}
	var chunks []json.RawMessage
	if err = wireValue(object, "chunks", &chunks); err != nil {
		return nil, err
	}
	out.Chunks = make([]dtoChunk, len(chunks))
	for i, chunk := range chunks {
		out.Chunks[i], err = decodeWireChunk(chunk)
		if err != nil {
			return nil, err
		}
	}
	if err = wireValue(object, "completion", &out.Completion); err != nil {
		return nil, err
	}
	outerErr, err := wireRaw(object, "outer_error")
	if err != nil {
		return nil, err
	}
	streamErr, err := wireRaw(object, "stream_error")
	if err != nil {
		return nil, err
	}
	if string(outerErr) != "null" {
		out.OuterError, err = decodeWireError(outerErr, version, "")
		if err != nil {
			return nil, err
		}
	}
	if string(streamErr) != "null" {
		out.StreamError, err = decodeWireError(streamErr, version, "")
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
func decodeWireChunk(raw json.RawMessage) (dtoChunk, error) {
	object, err := wireObject(raw, "type", "text", "output_index", "index", "id", "name", "delta", "item", "finish_reason", "usage")
	if err != nil {
		return dtoChunk{}, err
	}
	var typ string
	if err = wireValue(object, "type", &typ); err != nil {
		return dtoChunk{}, err
	}
	allowed := map[string][]string{"text_delta": {"type", "text", "output_index"}, "reasoning_delta": {"type", "text"}, "tool_call_start": {"type", "index", "id", "name"}, "tool_call_args": {"type", "index", "id", "delta"}, "reasoning_item": {"type", "output_index", "item"}, "done": {"type", "finish_reason", "usage"}}
	if !wireOnly(object, allowed[typ]) {
		return dtoChunk{}, incompatiblef("unknown chunk type or field %q", typ)
	}
	var out dtoChunk
	out.Type = typ
	switch typ {
	case "text_delta":
		err = wireValue(object, "text", &out.Text)
		if err == nil {
			err = wireValue(object, "output_index", &out.OutputIndex)
		}
	case "reasoning_delta":
		err = wireValue(object, "text", &out.Text)
	case "tool_call_start":
		err = wireValue(object, "index", &out.Index)
		if err == nil {
			err = wireValue(object, "id", &out.ID)
		}
		if err == nil {
			err = wireValue(object, "name", &out.Name)
		}
	case "tool_call_args":
		err = wireValue(object, "index", &out.Index)
		if err == nil {
			err = wireValue(object, "id", &out.ID)
		}
		if err == nil {
			err = wireValue(object, "delta", &out.Delta)
		}
	case "reasoning_item":
		err = wireValue(object, "output_index", &out.OutputIndex)
		if err == nil {
			item, itemErr := wireRaw(object, "item")
			if itemErr != nil {
				err = itemErr
			} else if string(item) != "null" {
				value, decodeErr := decodeWireReasoningItem(item)
				out.Item, err = &value, decodeErr
			} else {
				err = incompatiblef("reasoning item is null")
			}
		}
	case "done":
		err = wireValue(object, "finish_reason", &out.FinishReason)
		if err == nil {
			usage, usageErr := wireRaw(object, "usage")
			if usageErr != nil {
				err = usageErr
			} else if string(usage) != "null" {
				value, decodeErr := decodeWireUsage(usage)
				out.Usage, err = &value, decodeErr
			}
		}
	}
	if err != nil {
		return dtoChunk{}, err
	}
	return out, nil
}
func decodeWireUsage(raw json.RawMessage) (llm.Usage, error) {
	object, err := wireObject(raw, "InputTokens", "OutputTokens", "ReasoningTokens")
	if err != nil {
		return llm.Usage{}, err
	}
	var out llm.Usage
	if err = wireValue(object, "InputTokens", &out.InputTokens); err == nil {
		err = wireValue(object, "OutputTokens", &out.OutputTokens)
	}
	if err == nil {
		err = wireValue(object, "ReasoningTokens", &out.ReasoningTokens)
	}
	return out, err
}
func wireObject(raw json.RawMessage, allowed ...string) (map[string]json.RawMessage, error) {
	var out map[string]json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, incompatiblef("object: %v", err)
	}
	if out == nil {
		return nil, incompatiblef("object required")
	}
	if !wireOnly(out, allowed) {
		return nil, incompatiblef("unknown field")
	}
	return out, nil
}
func wireOnly(object map[string]json.RawMessage, allowed []string) bool {
	permitted := make(map[string]bool, len(allowed))
	for _, key := range allowed {
		permitted[key] = true
	}
	for key := range object {
		if !permitted[key] {
			return false
		}
	}
	return true
}
func wireValue(object map[string]json.RawMessage, key string, target any) error {
	raw, ok := object[key]
	if !ok {
		return incompatiblef("missing %s", key)
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return incompatiblef("%s: %v", key, err)
	}
	return nil
}
func wireOptional(object map[string]json.RawMessage, key string, target any) error {
	raw, ok := object[key]
	if !ok {
		return nil
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return incompatiblef("%s: %v", key, err)
	}
	return nil
}
