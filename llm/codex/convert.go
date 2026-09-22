package codex

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"strings"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

func convertMessages(messages []llm.Message) ([]inputItem, string, error) {
	items := make([]inputItem, 0)
	var instructions []string
	for _, m := range messages {
		messageStart := len(items)
		switch m.Role {
		case llm.RoleSystem, llm.RoleUser, llm.RoleAssistant, llm.RoleTool:
		default:
			return nil, "", capabilityError("message_role")
		}
		for _, block := range m.Content {
			if block == nil {
				return nil, "", capabilityError("nil_block")
			}
			value := reflect.ValueOf(block)
			if value.Kind() == reflect.Pointer {
				if value.IsNil() {
					return nil, "", capabilityError("nil_block")
				}
				block = value.Elem().Interface().(llm.ContentBlock)
			}
			var item inputItem
			switch b := block.(type) {
			case llm.TextBlock:
				if m.Role == llm.RoleSystem {
					instructions = append(instructions, b.Text)
					continue
				}
				if m.Role != llm.RoleUser && m.Role != llm.RoleAssistant {
					return nil, "", capabilityError("text_role")
				}
				kind := "input_text"
				if m.Role == llm.RoleAssistant {
					kind = "output_text"
				}
				item = inputItem{Type: "message", Role: string(m.Role), Content: []contentPart{{Type: kind, Text: &b.Text}}}
			case llm.ImageBlock:
				if m.Role != llm.RoleUser {
					return nil, "", capabilityError("image_role")
				}
				source := b.URL
				if source == "" && strings.HasPrefix(b.MIMEType, "image/") && len(b.Data) > 0 {
					source = "data:" + b.MIMEType + ";base64," + base64.StdEncoding.EncodeToString(b.Data)
				}
				u, err := url.Parse(source)
				if err != nil || source == "" || (u.Scheme != "https" && u.Scheme != "http" && u.Scheme != "data") ||
					((u.Scheme == "https" || u.Scheme == "http") && u.Host == "") {
					return nil, "", capabilityError("image_source")
				}
				if u.Scheme == "data" {
					header, data, ok := strings.Cut(source, ",")
					if !ok || !strings.HasPrefix(header, "data:image/") || !strings.HasSuffix(header, ";base64") {
						return nil, "", capabilityError("image_source")
					}
					decoded, err := base64.StdEncoding.DecodeString(data)
					if err != nil || len(decoded) == 0 {
						return nil, "", capabilityError("image_source")
					}
				}
				item = inputItem{Type: "message", Role: "user", Content: []contentPart{{Type: "input_image", ImageURL: source}}}
			case llm.ReasoningBlock:
				if m.Role != llm.RoleAssistant {
					return nil, "", capabilityError("reasoning_role")
				}
				continue
			case llm.ReasoningItemBlock:
				if m.Role != llm.RoleAssistant || b.ID == "" {
					return nil, "", capabilityError("reasoning_item")
				}
				item = inputItem{Type: "reasoning", ID: b.ID, EncryptedContent: b.EncryptedContent}
				for _, text := range b.Summary {
					item.Summary = append(item.Summary, summaryPart{Type: "summary_text", Text: text})
				}
			case llm.ToolUseBlock:
				if m.Role != llm.RoleAssistant || b.ID == "" || b.Name == "" || !json.Valid(b.Input) {
					return nil, "", capabilityError("tool_use")
				}
				args := string(b.Input)
				item = inputItem{Type: "function_call", CallID: b.ID, Name: b.Name, Arguments: &args}
			case llm.ToolResultBlock:
				if m.Role != llm.RoleTool || b.ToolUseID == "" {
					return nil, "", capabilityError("tool_result")
				}
				item = inputItem{Type: "function_call_output", CallID: b.ToolUseID, Output: &b.Content}
			default:
				return nil, "", capabilityError("content_block")
			}
			// Only adjacent parts of the same message are grouped.
			if item.Type == "message" && len(items) > messageStart && items[len(items)-1].Type == "message" &&
				items[len(items)-1].Role == item.Role {
				items[len(items)-1].Content = append(items[len(items)-1].Content, item.Content...)
			} else {
				items = append(items, item)
			}
		}
	}
	return items, strings.Join(instructions, "\n\n"), nil
}

func convertTools(tools []tool.ToolInfo) ([]toolDef, error) {
	defs := make([]toolDef, 0, len(tools))
	for _, t := range tools {
		parameters, err := json.Marshal(t.Parameters)
		if err != nil {
			return nil, fmt.Errorf("marshal tool schema: %w", err)
		}
		defs = append(defs, toolDef{Type: "function", Name: t.Name, Description: t.Description, Parameters: parameters})
	}
	return defs, nil
}
