package openairesponses

import (
	"encoding/json"
	"errors"
	"net/http"

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
	Message string                     `json:"message"`
	Code    string                     `json:"code"`
	Type    string                     `json:"type"`
	Param   string                     `json:"param"`
	Details map[string]json.RawMessage `json:"-"`
}

func (e *errorEnvelope) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	*e = errorEnvelope{Details: fields}
	for _, field := range []struct {
		name  string
		value *string
	}{
		{"message", &e.Message}, {"code", &e.Code}, {"type", &e.Type}, {"param", &e.Param},
	} {
		var text string
		if err := json.Unmarshal(fields[field.name], &text); err == nil {
			*field.value = text
		}
	}
	return nil
}

func (e *errorEnvelope) Error() string { return e.Message }

func classifyHTTPError(err error) error {
	// Do not classify partial bodies carried by joined transport errors.
	api, ok := err.(*llm.APIError)
	if !ok {
		return err
	}
	var envelope struct {
		Error *errorEnvelope `json:"error"`
	}
	if json.Unmarshal([]byte(api.Body), &envelope) == nil && envelope.Error != nil {
		cause := errors.Join(err, envelope.Error)
		if api.StatusCode == http.StatusBadRequest {
			return markContextOverflow(envelope.Error.Code, cause)
		}
		return cause
	}
	return err
}

func markContextOverflow(code string, err error) error {
	if code == "context_length_exceeded" {
		return &llm.ContextOverflowError{Code: code, Err: err}
	}
	return err
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
func reasoningItemOf(item outputItem) *llm.ReasoningItemBlock {
	b := &llm.ReasoningItemBlock{Type: "reasoning_item", ID: item.ID, EncryptedContent: item.EncryptedContent}
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
			content = append(content, reasoningItemOf(item))
		case "message":
			for _, part := range item.Content {
				if part.Type == "output_text" {
					content = append(content, &llm.TextBlock{Type: "text", Text: part.Text})
				}
			}
		case "function_call":
			content = append(content, &llm.ToolUseBlock{
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
