package agenttest

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

func TestScriptedProviderClonesWithoutNormalization(t *testing.T) {
	invalid := json.RawMessage{'{'}
	input := []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolUseBlock{Input: invalid}}}}
	tools := []tool.ToolInfo{{Name: "tool", Parameters: tool.ParameterSchema{Properties: map[string]tool.Property{"p": {Enum: []string{}}}, Required: []string{}}}}
	provider := NewScriptedProvider(Exchange{Method: MethodChat, Request: Request{Messages: input, Tools: tools}})
	input[0].Content[0] = llm.TextBlock{Text: "mutated"}
	tools[0].Parameters.Properties["p"] = tool.Property{Enum: []string{"mutated"}}
	original := []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolUseBlock{Input: invalid}}}}
	originalTools := []tool.ToolInfo{{Name: "tool", Parameters: tool.ParameterSchema{Properties: map[string]tool.Property{"p": {Enum: []string{}}}, Required: []string{}}}}
	if _, _, err := provider.Chat(t.Context(), original, originalTools); err != nil {
		t.Fatal(err)
	}
}

func TestScriptedProviderClonesPointerValues(t *testing.T) {
	block := &llm.ToolUseBlock{Input: json.RawMessage(`{"x":1}`)}
	chunk := &llm.ReasoningItemChunk{Item: llm.ReasoningItemBlock{Summary: []string{"original"}}}
	provider := NewScriptedProvider(Exchange{
		Method:       MethodChatStream,
		Request:      Request{Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{block}}}},
		StreamChunks: []llm.Chunk{chunk},
	})
	block.Input[2] = 'y'
	chunk.Item.Summary[0] = "mutated"
	sequence, err := provider.ChatStream(t.Context(), []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{&llm.ToolUseBlock{Input: json.RawMessage(`{"x":1}`)}}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for got := range sequence {
		item := got.(*llm.ReasoningItemChunk)
		if item == chunk || item.Item.Summary[0] != "original" {
			t.Fatalf("pointer chunk was aliased: %#v", item)
		}
	}
}

func TestStrictRequestPreservesNilAndEmptyValues(t *testing.T) {
	request := Request{Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ImageBlock{Data: []byte{}}}}}, Tools: []tool.ToolInfo{}}
	provider := NewScriptedProvider(Exchange{Method: MethodChat, Request: request})
	actual := []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ImageBlock{Data: nil}}}}
	_, _, err := provider.Chat(t.Context(), actual, nil)
	if !errors.Is(err, ErrScriptMismatch) {
		t.Fatalf("nil/empty request matched: %v", err)
	}
}
