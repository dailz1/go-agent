package openairesponses

import (
	"encoding/json"

	"github.com/dailz1/go-agent/llm"
)

// --- response shapes ---

type responseEnvelope struct {
	ID                string          `json:"id"`
	Status            string          `json:"status"`
	Output            []outputItem    `json:"output"`
	Usage             usageEnvelope   `json:"usage"`
	Error             *errorEnvelope  `json:"error"`
	IncompleteDetails *incompleteSpec `json:"incomplete_details"`
}

type errorEnvelope struct {
	Message string `json:"message"`
}

type incompleteSpec struct {
	Reason string `json:"reason"`
}

type usageEnvelope struct {
	InputTokens     int64 `json:"input_tokens"`
	OutputTokens    int64 `json:"output_tokens"`
	TotalTokens     int64 `json:"total_tokens"`
	ReasoningTokens struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

func (u usageEnvelope) toUsage() llm.Usage {
	return llm.Usage{
		InputTokens:     u.InputTokens,
		OutputTokens:    u.OutputTokens,
		ReasoningTokens: u.ReasoningTokens.ReasoningTokens,
	}
}

// outputItem is one typed entry of the output array.
type outputItem struct {
	Type             string        `json:"type"`
	ID               string        `json:"id"`
	Role             string        `json:"role"`
	CallID           string        `json:"call_id"`
	Name             string        `json:"name"`
	Arguments        string        `json:"arguments"`
	EncryptedContent string        `json:"encrypted_content"`
	Summary          []summaryPart `json:"summary"`
	Content          []contentOut  `json:"content"`
}

type contentOut struct {
	Type string `json:"type"` // output_text | refusal
	Text string `json:"text"`
}

// reasoningItemOf converts a reasoning output item into the library block,
// preserving the server-side ID, encrypted content, and summary texts.
func reasoningItemOf(item outputItem) llm.ReasoningItemBlock {
	b := llm.ReasoningItemBlock{Type: "reasoning_item", ID: item.ID, EncryptedContent: item.EncryptedContent}
	for _, s := range item.Summary {
		b.Summary = append(b.Summary, s.Text)
	}
	return b
}

// outputToMessage assembles the assistant message from a completed
// response's output items. Reasoning items become ReasoningItemBlocks
// (preserving id and encrypted content), message items contribute their
// output_text parts, and function_call items become ToolUseBlocks. Unknown
// item types are skipped for forward compatibility.
func outputToMessage(logger logger, output []outputItem) llm.Message {
	var content []llm.ContentBlock
	for _, item := range output {
		switch item.Type {
		case "reasoning":
			b := llm.ReasoningItemBlock{Type: "reasoning_item", ID: item.ID, EncryptedContent: item.EncryptedContent}
			for _, s := range item.Summary {
				b.Summary = append(b.Summary, s.Text)
			}
			content = append(content, b)
		case "message":
			for _, part := range item.Content {
				if part.Type == "output_text" {
					content = append(content, llm.TextBlock{Type: "text", Text: part.Text})
				}
			}
		case "function_call":
			content = append(content, llm.ToolUseBlock{
				Type:  "tool_use",
				ID:    item.CallID,
				Name:  item.Name,
				Input: json.RawMessage(item.Arguments),
			})
		default:
			logger.Debug("openairesponses: ignoring unknown output item type", "type", item.Type)
		}
	}
	return llm.Message{Role: llm.RoleAssistant, Content: content}
}
