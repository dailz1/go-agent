package agenttest

import (
	"encoding/json"
	"sort"

	"github.com/dailz1/go-agent/pkg/llm"
	"github.com/dailz1/go-agent/pkg/tool"
)

func cloneExchange(e Exchange) Exchange {
	return Exchange{Method: e.Method, Request: Request{Messages: cloneMessages(e.Request.Messages), Tools: cloneTools(e.Request.Tools), Options: cloneOptions(e.Request.Options)}, ChatResponse: cloneMessagePtr(e.ChatResponse), ChatUsage: cloneUsage(e.ChatUsage), ChatErr: e.ChatErr, StreamChunks: cloneChunks(e.StreamChunks), StreamOuterErr: e.StreamOuterErr, StreamErr: e.StreamErr}
}

func cloneMessages(in []llm.Message) []llm.Message {
	if in == nil {
		return nil
	}
	out := make([]llm.Message, len(in))
	for i, message := range in {
		out[i] = llm.Message{Role: message.Role, Content: cloneBlocks(message.Content)}
	}
	return out
}
func cloneBlocks(in []llm.ContentBlock) []llm.ContentBlock {
	if in == nil {
		return nil
	}
	out := make([]llm.ContentBlock, len(in))
	for i, block := range in {
		switch value := block.(type) {
		case llm.TextBlock:
			out[i] = value
		case *llm.TextBlock:
			out[i] = cloneTextBlock(value)
		case llm.ReasoningBlock:
			out[i] = value
		case *llm.ReasoningBlock:
			out[i] = cloneReasoningBlock(value)
		case llm.ImageBlock:
			value.Data = cloneBytes(value.Data)
			out[i] = value
		case *llm.ImageBlock:
			out[i] = cloneImageBlock(value)
		case llm.ToolUseBlock:
			value.Input = cloneRaw(value.Input)
			out[i] = value
		case *llm.ToolUseBlock:
			out[i] = cloneToolUseBlock(value)
		case llm.ReasoningItemBlock:
			value.Summary = cloneStrings(value.Summary)
			out[i] = value
		case *llm.ReasoningItemBlock:
			out[i] = cloneReasoningItemBlock(value)
		case llm.ToolResultBlock:
			out[i] = value
		case *llm.ToolResultBlock:
			out[i] = cloneToolResultBlock(value)
		default:
			out[i] = block
		}
	}
	return out
}
func cloneMessagePtr(in *llm.Message) *llm.Message {
	if in == nil {
		return nil
	}
	out := cloneMessages([]llm.Message{*in})
	return &out[0]
}
func cloneUsage(in *llm.Usage) *llm.Usage {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}
func cloneOptions(in llm.Options) llm.Options {
	out := in
	if in.Temperature != nil {
		value := *in.Temperature
		out.Temperature = &value
	}
	out.Stop = cloneStrings(in.Stop)
	return out
}
func cloneTools(in []tool.ToolInfo) []tool.ToolInfo {
	if in == nil {
		return nil
	}
	out := make([]tool.ToolInfo, len(in))
	for i, info := range in {
		out[i] = tool.ToolInfo{Name: info.Name, Description: info.Description, RequiresApproval: info.RequiresApproval, Parameters: cloneSchema(info.Parameters)}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
func cloneSchema(in tool.ParameterSchema) tool.ParameterSchema {
	out := tool.ParameterSchema{Type: in.Type, Required: cloneStrings(in.Required)}
	if in.Properties != nil {
		out.Properties = make(map[string]tool.Property, len(in.Properties))
		for name, property := range in.Properties {
			property.Enum = cloneStrings(property.Enum)
			out.Properties[name] = property
		}
	}
	return out
}
func cloneChunks(in []llm.Chunk) []llm.Chunk {
	if in == nil {
		return nil
	}
	out := make([]llm.Chunk, len(in))
	for i, chunk := range in {
		out[i] = cloneChunk(chunk)
	}
	return out
}
func cloneChunk(chunk llm.Chunk) llm.Chunk {
	switch value := chunk.(type) {
	case llm.TextDeltaChunk:
		return value
	case *llm.TextDeltaChunk:
		return cloneTextDeltaChunk(value)
	case llm.ReasoningDeltaChunk:
		return value
	case *llm.ReasoningDeltaChunk:
		return cloneReasoningDeltaChunk(value)
	case llm.ToolCallStartChunk:
		return value
	case *llm.ToolCallStartChunk:
		return cloneToolCallStartChunk(value)
	case llm.ToolCallArgsChunk:
		return value
	case *llm.ToolCallArgsChunk:
		return cloneToolCallArgsChunk(value)
	case llm.ReasoningItemChunk:
		value.Item.Summary = cloneStrings(value.Item.Summary)
		return value
	case *llm.ReasoningItemChunk:
		return cloneReasoningItemChunk(value)
	case llm.DoneChunk:
		value.Usage = cloneUsage(value.Usage)
		return value
	case *llm.DoneChunk:
		return cloneDoneChunk(value)
	default:
		return chunk
	}
}
func cloneStrings(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}
func cloneBytes(in []byte) []byte {
	if in == nil {
		return nil
	}
	out := make([]byte, len(in))
	copy(out, in)
	return out
}
func cloneRaw(in json.RawMessage) json.RawMessage { return json.RawMessage(cloneBytes(in)) }
