package agenttest

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

func decodeRecording(data []byte) (dtoRecording, error) {
	root, err := wireObject(data, "version", "exchanges")
	if err != nil {
		return dtoRecording{}, err
	}
	var version int
	var exchanges []json.RawMessage
	if err := wireValue(root, "version", &version); err != nil {
		return dtoRecording{}, err
	}
	if err := wireValue(root, "exchanges", &exchanges); err != nil {
		return dtoRecording{}, err
	}
	if version != 1 && version != 2 && version != 3 {
		return dtoRecording{}, incompatiblef("unsupported recording version %d", version)
	}
	out := dtoRecording{Version: version, Exchanges: make([]dtoExchange, len(exchanges))}
	for i, raw := range exchanges {
		value, err := decodeWireExchange(raw, version)
		if err != nil {
			return dtoRecording{}, fmt.Errorf("exchange %d: %w", i, err)
		}
		out.Exchanges[i] = value
	}
	return out, nil
}
func decodeWireExchange(raw json.RawMessage, version int) (dtoExchange, error) {
	object, err := wireObject(raw, "method", "request", "chat", "stream")
	if err != nil {
		return dtoExchange{}, err
	}
	var method Method
	if err := wireValue(object, "method", &method); err != nil {
		return dtoExchange{}, err
	}
	request, err := decodeWireRequest(object["request"], version)
	if err != nil {
		return dtoExchange{}, err
	}
	out := dtoExchange{Method: method, Request: request}
	if raw := object["chat"]; raw != nil {
		out.Chat, err = decodeWireChat(raw, version)
		if err != nil {
			return dtoExchange{}, err
		}
	}
	if raw := object["stream"]; raw != nil {
		out.Stream, err = decodeWireStream(raw, version)
		if err != nil {
			return dtoExchange{}, err
		}
	}
	return out, nil
}
func decodeWireRequest(raw json.RawMessage, version int) (dtoRequest, error) {
	object, err := wireObject(raw, "messages", "tools", "options")
	if err != nil {
		return dtoRequest{}, err
	}
	var messages []json.RawMessage
	var tools []json.RawMessage
	if err := wireValue(object, "messages", &messages); err != nil {
		return dtoRequest{}, err
	}
	if err := wireValue(object, "tools", &tools); err != nil {
		return dtoRequest{}, err
	}
	options, err := decodeWireOptions(object["options"])
	if err != nil {
		return dtoRequest{}, err
	}
	out := dtoRequest{Options: options}
	if messages != nil {
		out.Messages = make([]llm.Message, len(messages))
	}
	if tools != nil {
		out.Tools = make([]tool.ToolInfo, len(tools))
	}
	for i, raw := range messages {
		out.Messages[i], err = decodeWireMessage(raw)
		if err != nil {
			return dtoRequest{}, err
		}
	}
	for i, raw := range tools {
		out.Tools[i], err = decodeWireTool(raw, version)
		if err != nil {
			return dtoRequest{}, err
		}
	}
	return out, nil
}
func decodeWireOptions(raw json.RawMessage) (llm.Options, error) {
	object, err := wireObject(raw, "Model", "MaxTokens", "Temperature", "Stop")
	if err != nil {
		return llm.Options{}, err
	}
	var out llm.Options
	if err = wireValue(object, "Model", &out.Model); err != nil {
		return out, err
	}
	if err = wireValue(object, "MaxTokens", &out.MaxTokens); err != nil {
		return out, err
	}
	if err = wireValue(object, "Temperature", &out.Temperature); err != nil {
		return out, err
	}
	if err = wireValue(object, "Stop", &out.Stop); err != nil {
		return out, err
	}
	return out, nil
}
func decodeWireMessage(raw json.RawMessage) (llm.Message, error) {
	object, err := wireObject(raw, "role", "content")
	if err != nil {
		return llm.Message{}, err
	}
	var out llm.Message
	if err = wireValue(object, "role", &out.Role); err != nil {
		return out, err
	}
	if bytes.Equal(object["content"], []byte("null")) {
		return out, nil
	}
	var blocks []json.RawMessage
	if err = json.Unmarshal(object["content"], &blocks); err != nil {
		return out, incompatiblef("message content: %v", err)
	}
	out.Content = make([]llm.ContentBlock, len(blocks))
	for i, block := range blocks {
		out.Content[i], err = decodeWireBlock(block)
		if err != nil {
			return out, err
		}
	}
	return out, nil
}
func decodeWireBlock(raw json.RawMessage) (llm.ContentBlock, error) {
	object, err := wireObject(raw, "type", "text", "content", "url", "mime_type", "data", "id", "name", "input", "encrypted_content", "summary", "tool_use_id", "is_error")
	if err != nil {
		return nil, err
	}
	var typ string
	if err = wireValue(object, "type", &typ); err != nil {
		return nil, err
	}
	allowed := map[string][]string{"text": {"type", "text"}, "reasoning": {"type", "content"}, "image": {"type", "url", "mime_type", "data"}, "tool_use": {"type", "id", "name", "input"}, "reasoning_item": {"type", "id", "encrypted_content", "summary"}, "tool_result": {"type", "tool_use_id", "content", "is_error"}}
	if !wireOnly(object, allowed[typ]) {
		return nil, incompatiblef("unknown content block type or field %q", typ)
	}
	switch typ {
	case "text":
		value := llm.TextBlock{Type: typ}
		err = wireValue(object, "text", &value.Text)
		return value, err
	case "reasoning":
		value := llm.ReasoningBlock{Type: typ}
		err = wireValue(object, "content", &value.Content)
		return value, err
	case "image":
		value := llm.ImageBlock{Type: typ}
		if err = wireOptional(object, "url", &value.URL); err == nil {
			err = wireOptional(object, "mime_type", &value.MIMEType)
		}
		if err == nil {
			err = wireOptional(object, "data", &value.Data)
		}
		return value, err
	case "tool_use":
		value := llm.ToolUseBlock{Type: typ}
		if err = wireValue(object, "id", &value.ID); err == nil {
			err = wireValue(object, "name", &value.Name)
		}
		if err == nil {
			err = wireValue(object, "input", &value.Input)
		}
		return value, err
	case "reasoning_item":
		value, err := decodeWireReasoningItem(raw)
		return value, err
	case "tool_result":
		value := llm.ToolResultBlock{Type: typ}
		if err = wireValue(object, "tool_use_id", &value.ToolUseID); err == nil {
			err = wireValue(object, "content", &value.Content)
		}
		if err == nil {
			err = wireOptional(object, "is_error", &value.IsError)
		}
		return value, err
	}
	return nil, incompatiblef("unknown content block type %q", typ)
}
func decodeWireReasoningItem(raw json.RawMessage) (llm.ReasoningItemBlock, error) {
	var value llm.ReasoningItemBlock
	object, err := wireObject(raw, "type", "id", "encrypted_content", "summary")
	if err != nil {
		return value, err
	}
	if err = wireValue(object, "type", &value.Type); err != nil {
		return value, err
	}
	if value.Type != "reasoning_item" {
		return value, incompatiblef("unknown reasoning item type %q", value.Type)
	}
	if err = wireOptional(object, "id", &value.ID); err == nil {
		err = wireOptional(object, "encrypted_content", &value.EncryptedContent)
	}
	if err == nil {
		err = wireOptional(object, "summary", &value.Summary)
	}
	return value, err
}
