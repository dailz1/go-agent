package llm

import (
	"encoding/json"
	"fmt"

	"github.com/dailz1/go-agent/pkg/tool"
)

// Role represents who is speaking in the conversation.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is a provider-agnostic conversation message.
// All content (text, images, tool calls, tool results) is expressed as ContentBlock variants.
type Message struct {
	Role    Role           `json:"role"`
	Content []ContentBlock `json:"content"`
}

// ContentBlock is a sealed interface — only types defined in this package can implement it.
// This gives compile-time type safety and exhaustive switch checking.
type ContentBlock interface {
	blockType() string
}

// TextBlock represents a plain text content.
type TextBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func (TextBlock) blockType() string { return "text" }

// ReasoningBlock represents model reasoning/thinking content.
type ReasoningBlock struct {
	Type    string `json:"type"`
	Content string `json:"content"`
}

func (ReasoningBlock) blockType() string { return "reasoning" }

// ImageBlock represents an image, either by URL or inline base64 data.
type ImageBlock struct {
	Type     string `json:"type"`
	URL      string `json:"url,omitempty"`
	MIMEType string `json:"mime_type,omitempty"`
	Data     []byte `json:"data,omitempty"`
}

func (ImageBlock) blockType() string { return "image" }

// ToolUseBlock represents an LLM's request to invoke a tool.
type ToolUseBlock struct {
	Type  string          `json:"type"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

func (ToolUseBlock) blockType() string { return "tool_use" }

// ReasoningItemBlock is an entry-level reasoning item from the Responses
// protocol: the model's reasoning tokens as a distinct, addressable item
// (with its server-side ID and, for stateless store:false usage, the opaque
// encrypted content) that must be replayed ahead of the tool calls it
// precedes. It is protocol-specific: Chat Completions adapters skip it.
type ReasoningItemBlock struct {
	Type             string   `json:"type"`
	ID               string   `json:"id,omitempty"`
	EncryptedContent string   `json:"encrypted_content,omitempty"`
	Summary          []string `json:"summary,omitempty"`
}

func (ReasoningItemBlock) blockType() string { return "reasoning_item" }

// ToolResultBlock represents the result of a tool execution, fed back to the LLM.
type ToolResultBlock struct {
	Type      string `json:"type"`
	ToolUseID string `json:"tool_use_id"`
	Content   string `json:"content"`
	IsError   bool   `json:"is_error,omitempty"`
}

func (ToolResultBlock) blockType() string { return "tool_result" }

func SystemMessage(text string) Message {
	return Message{
		Role:    RoleSystem,
		Content: []ContentBlock{TextBlock{Type: "text", Text: text}},
	}
}

func UserMessage(text string) Message {
	return Message{
		Role:    RoleUser,
		Content: []ContentBlock{TextBlock{Type: "text", Text: text}},
	}
}

func AssistantMessage(text string) Message {
	return Message{
		Role:    RoleAssistant,
		Content: []ContentBlock{TextBlock{Type: "text", Text: text}},
	}
}

func AssistantToolCallMessage(calls ...ToolUseBlock) Message {
	blocks := make([]ContentBlock, len(calls))
	for i, call := range calls {
		call.Type = "tool_use"
		blocks[i] = call
	}
	return Message{
		Role:    RoleAssistant,
		Content: blocks,
	}
}

func ToolResultMessage(toolUseID string, result *tool.ToolResult) Message {
	if result == nil {
		result = tool.NewErrorResult("tool returned nil result")
	}
	return Message{
		Role: RoleTool,
		Content: []ContentBlock{
			ToolResultBlock{
				Type:      "tool_result",
				ToolUseID: toolUseID,
				Content:   result.Content,
				IsError:   result.IsError(),
			},
		},
	}
}

// UnmarshalJSON implements custom deserialization for Message to reconstruct
// the concrete ContentBlock types from the "type" discriminator.
// It handles content as a JSON array (standard), a JSON string (OpenAI compatibility),
// or JSON null.
func (m *Message) UnmarshalJSON(data []byte) error {
	var raw struct {
		Role    Role            `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	m.Role = raw.Role

	// Handle null or missing content.
	if len(raw.Content) == 0 || string(raw.Content) == "null" {
		m.Content = []ContentBlock{}
		return nil
	}

	switch raw.Content[0] {
	case '"':
		var s string
		if err := json.Unmarshal(raw.Content, &s); err != nil {
			return fmt.Errorf("unmarshal content string: %w", err)
		}
		m.Content = []ContentBlock{TextBlock{Type: "text", Text: s}}
		return nil
	case '[':
		var rawBlocks []json.RawMessage
		if err := json.Unmarshal(raw.Content, &rawBlocks); err != nil {
			return fmt.Errorf("unmarshal content array: %w", err)
		}
		blocks := make([]ContentBlock, 0, len(rawBlocks))
		for _, rawBlock := range rawBlocks {
			b, err := unmarshalContentBlock(rawBlock)
			if err != nil {
				return fmt.Errorf("unmarshal content block: %w", err)
			}
			blocks = append(blocks, b)
		}
		m.Content = blocks
		return nil
	default:
		return fmt.Errorf("content must be a string, array, or null, got %q", string(raw.Content))
	}
}

func unmarshalContentBlock(data []byte) (ContentBlock, error) {
	var disc struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &disc); err != nil {
		return nil, err
	}

	switch disc.Type {
	case "text":
		var b TextBlock
		return b, json.Unmarshal(data, &b)
	case "reasoning":
		var b ReasoningBlock
		return b, json.Unmarshal(data, &b)
	case "reasoning_item":
		var b ReasoningItemBlock
		return b, json.Unmarshal(data, &b)
	case "image":
		var b ImageBlock
		return b, json.Unmarshal(data, &b)
	case "tool_use":
		var b ToolUseBlock
		return b, json.Unmarshal(data, &b)
	case "tool_result":
		var b ToolResultBlock
		return b, json.Unmarshal(data, &b)
	default:
		return nil, fmt.Errorf("unknown content block type: %q", disc.Type)
	}
}
