package openairesponses

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/dailz1/go-agent/pkg/llm"
	"github.com/dailz1/go-agent/pkg/tool"
)

// --- request shapes ---

type createRequest struct {
	Model           string      `json:"model"`
	Input           []inputItem `json:"input"`
	Instructions    string      `json:"instructions,omitempty"`
	Tools           []toolDef   `json:"tools,omitempty"`
	Store           bool        `json:"store"`
	Include         []string    `json:"include,omitempty"`
	Stream          bool        `json:"stream,omitempty"`
	MaxOutputTokens int         `json:"max_output_tokens,omitempty"`
	// Temperature is a pointer so an explicit WithTemperature(0) stays on the
	// wire — a plain float64 with omitempty would silently drop it.
	Temperature *float64 `json:"temperature,omitempty"`
}

// inputItem is one typed entry of the input array. A single flat struct with
// omitempty fields covers the four item kinds the adapter produces: message,
// function_call, function_call_output, and reasoning.
type inputItem struct {
	Type             string        `json:"type"`
	Role             string        `json:"role,omitempty"`
	Content          []contentPart `json:"content,omitempty"`
	CallID           string        `json:"call_id,omitempty"`
	Name             string        `json:"name,omitempty"`
	Arguments        string        `json:"arguments,omitempty"`
	Output           string        `json:"output,omitempty"`
	ID               string        `json:"id,omitempty"`
	EncryptedContent string        `json:"encrypted_content,omitempty"`
	Summary          []summaryPart `json:"summary,omitempty"`
}

type contentPart struct {
	Type string `json:"type"` // input_text | output_text | input_image
	Text string `json:"text,omitempty"`
	// ImageURL carries an image reference for input_image parts.
	ImageURL string `json:"image_url,omitempty"`
}

type summaryPart struct {
	Type string `json:"type"` // always summary_text
	Text string `json:"text"`
}

type toolDef struct {
	Type        string          `json:"type"` // always function
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      bool            `json:"strict"`
}

// --- messages → input items ---

// convertMessages maps the internal message history to Responses input items.
// System messages become the top-level instructions string (the last one
// wins, matching single-instruction semantics); an assistant message fans
// out into reasoning / message / function_call items preserving its block
// order; a tool message becomes one function_call_output item.
func convertMessages(messages []llm.Message) (items []inputItem, instructions string, err error) {
	for _, m := range messages {
		switch m.Role {
		case llm.RoleSystem:
			if t := messageText(m); t != "" {
				instructions = t
			}
		case llm.RoleUser:
			item, convErr := userItem(m)
			if convErr != nil {
				return nil, "", convErr
			}
			items = append(items, item)
		case llm.RoleAssistant:
			out, convErr := assistantItems(m)
			if convErr != nil {
				return nil, "", convErr
			}
			items = append(items, out...)
		case llm.RoleTool:
			item, convErr := toolOutputItem(m)
			if convErr != nil {
				return nil, "", convErr
			}
			items = append(items, item)
		default:
			return nil, "", fmt.Errorf("unsupported message role %q", m.Role)
		}
	}
	return items, instructions, nil
}

func userItem(m llm.Message) (inputItem, error) {
	item := inputItem{Type: "message", Role: string(llm.RoleUser)}
	for _, blk := range m.Content {
		switch b := blk.(type) {
		case llm.TextBlock:
			item.Content = append(item.Content, contentPart{Type: "input_text", Text: b.Text})
		case llm.ImageBlock:
			url := b.URL
			if url == "" && b.MIMEType != "" && len(b.Data) > 0 {
				url = "data:" + b.MIMEType + ";base64," + base64.StdEncoding.EncodeToString(b.Data)
			}
			item.Content = append(item.Content, contentPart{Type: "input_image", ImageURL: url})
		default:
			return inputItem{}, fmt.Errorf("unsupported content block type %T for user message", b)
		}
	}
	return item, nil
}

// assistantItems fans one assistant message out into its item sequence.
// Block order is preserved: a reasoning_item block is emitted immediately
// (so it precedes the function_call items it belongs to), consecutive text
// blocks group into a single message item, and each tool_use block becomes
// its own function_call item.
func assistantItems(m llm.Message) ([]inputItem, error) {
	var items []inputItem
	var textParts []string
	flushText := func() {
		if len(textParts) > 0 {
			item := inputItem{Type: "message", Role: string(llm.RoleAssistant)}
			for _, t := range textParts {
				item.Content = append(item.Content, contentPart{Type: "output_text", Text: t})
			}
			items = append(items, item)
			textParts = nil
		}
	}
	for _, blk := range m.Content {
		switch b := blk.(type) {
		case llm.ReasoningItemBlock:
			flushText()
			item := inputItem{Type: "reasoning", ID: b.ID, EncryptedContent: b.EncryptedContent}
			for _, s := range b.Summary {
				item.Summary = append(item.Summary, summaryPart{Type: "summary_text", Text: s})
			}
			items = append(items, item)
		case *llm.ReasoningItemBlock:
			// Pointer form (deep-copied histories): same handling as value.
			if b == nil {
				continue
			}
			flushText()
			item := inputItem{Type: "reasoning", ID: b.ID, EncryptedContent: b.EncryptedContent}
			for _, s := range b.Summary {
				item.Summary = append(item.Summary, summaryPart{Type: "summary_text", Text: s})
			}
			items = append(items, item)
		case llm.ReasoningBlock:
			// Chat Completions-style embedded reasoning text: not part of
			// the Responses item model; drop it rather than corrupting the
			// item sequence.
		case llm.TextBlock:
			textParts = append(textParts, b.Text)
		case llm.ToolUseBlock:
			flushText()
			items = append(items, inputItem{
				Type:      "function_call",
				CallID:    b.ID,
				Name:      b.Name,
				Arguments: string(b.Input),
			})
		default:
			return nil, fmt.Errorf("unsupported content block type %T for assistant message", b)
		}
	}
	flushText()
	return items, nil
}

func toolOutputItem(m llm.Message) (inputItem, error) {
	for _, blk := range m.Content {
		if tr, ok := blk.(llm.ToolResultBlock); ok {
			return inputItem{
				Type:   "function_call_output",
				CallID: tr.ToolUseID,
				Output: tr.Content,
			}, nil
		}
	}
	return inputItem{}, fmt.Errorf("tool message has no tool_result block")
}

// convertToolDefs maps internal tool metadata to Responses function tool
// definitions. strict is set explicitly to false: Responses would otherwise
// attempt strict normalization with non-deterministic fallback.
func convertToolDefs(tools []tool.ToolInfo) ([]toolDef, error) {
	defs := make([]toolDef, 0, len(tools))
	for _, t := range tools {
		raw, err := json.Marshal(t.Parameters)
		if err != nil {
			return nil, fmt.Errorf("marshal parameters for %s: %w", t.Name, err)
		}
		defs = append(defs, toolDef{
			Type:        "function",
			Name:        t.Name,
			Description: t.Description,
			Parameters:  raw,
			Strict:      false,
		})
	}
	return defs, nil
}

// ErrStopUnsupported is returned when WithStop is used with the Responses
// adapter: the protocol has no stop-sequence parameter, and silently
// dropping a caller's explicit constraint is not acceptable.
var ErrStopUnsupported = errors.New("openairesponses: WithStop is not supported by the Responses protocol")

// buildRequest assembles the create-request body from the converted items
// and the per-call options. Supported common options map to their Responses
// equivalents; unsupported ones are rejected explicitly.
func buildRequest(o llm.Options, defaultModel string, items []inputItem, instructions string, defs []toolDef, stream bool) (createRequest, error) {
	if len(o.Stop) > 0 {
		return createRequest{}, ErrStopUnsupported
	}
	req := createRequest{
		Model:        firstNonEmpty(o.Model, defaultModel),
		Input:        items,
		Instructions: instructions,
		Tools:        defs,
		Store:        false,
		Include:      []string{"reasoning.encrypted_content"},
		Stream:       stream,
	}
	if o.MaxTokens > 0 {
		req.MaxOutputTokens = o.MaxTokens
	}
	if o.Temperature != nil {
		req.Temperature = o.Temperature
	}
	return req, nil
}

// --- output items → assistant message ---

func messageText(m llm.Message) string {
	var b strings.Builder
	for _, blk := range m.Content {
		if tb, ok := blk.(llm.TextBlock); ok {
			b.WriteString(tb.Text)
		}
	}
	return b.String()
}

// logger is the minimal logging surface the converter needs, satisfied by
// *slog.Logger.
type logger interface {
	Debug(msg string, args ...any)
}
