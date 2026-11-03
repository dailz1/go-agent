package codex

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/llm"
)

func TestConvertMessages(t *testing.T) {
	messages := []llm.Message{
		llm.SystemMessage("first"), llm.SystemMessage("second"),
		{Role: llm.RoleUser, Content: []llm.ContentBlock{&llm.TextBlock{Text: ""}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			&llm.ReasoningItemBlock{ID: "r", EncryptedContent: "opaque", Summary: []string{"summary"}},
			&llm.TextBlock{Text: "text"}, &llm.ToolUseBlock{ID: "call", Name: "f", Input: json.RawMessage(`[]`)},
		}},
		{Role: llm.RoleTool, Content: []llm.ContentBlock{
			&llm.ToolResultBlock{ToolUseID: "call", Content: "", IsError: true},
			llm.ToolResultBlock{ToolUseID: "other", Content: "result"},
		}},
	}
	items, instructions, err := convertMessages(messages)
	if err != nil || instructions != "first\n\nsecond" {
		t.Fatalf("instructions=%q err=%v", instructions, err)
	}
	data, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	var got []map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	wantTypes := []string{"message", "reasoning", "message", "function_call", "function_call_output", "function_call_output"}
	var types []string
	for _, item := range got {
		types = append(types, item["type"].(string))
	}
	if !reflect.DeepEqual(types, wantTypes) || got[4]["output"] != "" || got[3]["call_id"] != "call" {
		t.Fatalf("items=%s", data)
	}
	if got[0]["content"].([]any)[0].(map[string]any)["text"] != "" {
		t.Fatal("lost empty text")
	}
}

func TestConvertRejectsInvalidBlocks(t *testing.T) {
	for name, message := range map[string]llm.Message{
		"nil":           {Role: llm.RoleUser, Content: []llm.ContentBlock{(*llm.TextBlock)(nil)}},
		"system image":  {Role: llm.RoleSystem, Content: []llm.ContentBlock{llm.ImageBlock{URL: "https://example.com/a"}}},
		"missing image": {Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ImageBlock{}}},
		"user tool":     {Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolUseBlock{ID: "c"}}},
		"missing call":  {Role: llm.RoleTool, Content: []llm.ContentBlock{llm.ToolResultBlock{Content: "x"}}},
		"role":          {Role: "other"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := convertMessages([]llm.Message{message}); err == nil {
				t.Fatal("accepted invalid message")
			}
		})
	}
}

func TestConvertPreservesMessageBoundaries(t *testing.T) {
	items, _, err := convertMessages([]llm.Message{llm.UserMessage("first"), llm.UserMessage("second")})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("merged separate messages: %+v", items)
	}
}

func TestConvertImageSource(t *testing.T) {
	for _, tc := range []struct {
		name  string
		image llm.ImageBlock
		valid bool
	}{
		{"https", llm.ImageBlock{URL: "https://example.com/image"}, true},
		{"inline", llm.ImageBlock{MIMEType: "image/png", Data: []byte("test")}, true},
		{"data url", llm.ImageBlock{URL: "data:image/png;base64,dGVzdA=="}, true},
		{"garbage data", llm.ImageBlock{URL: "data:garbage"}, false},
		{"bad base64", llm.ImageBlock{URL: "data:image/png;base64,%%%%"}, false},
		{"empty data", llm.ImageBlock{URL: "data:image/png;base64,"}, false},
		{"non image", llm.ImageBlock{MIMEType: "text/plain", Data: []byte("test")}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := convertMessages([]llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{&tc.image}}})
			if (err == nil) != tc.valid {
				t.Fatalf("error=%v valid=%t", err, tc.valid)
			}
		})
	}
}

func TestConvertNilPointers(t *testing.T) {
	for _, block := range []llm.ContentBlock{
		(*llm.TextBlock)(nil), (*llm.ImageBlock)(nil), (*llm.ReasoningBlock)(nil),
		(*llm.ReasoningItemBlock)(nil), (*llm.ToolUseBlock)(nil), (*llm.ToolResultBlock)(nil), nil,
	} {
		if _, _, err := convertMessages([]llm.Message{{Role: llm.RoleAssistant, Content: []llm.ContentBlock{block}}}); err == nil {
			t.Fatalf("accepted nil %T", block)
		}
	}
}

// TestToolResultImageParts pins the codex tool-result union: images emit an
// ordered input_text + input_image array under the same image whitelist as
// user-side attachments; empty content without images keeps the pre-union
// `"output":""` byte shape.
func TestToolResultImageParts(t *testing.T) {
	messages := []llm.Message{
		{Role: llm.RoleTool, Content: []llm.ContentBlock{
			llm.ToolResultBlock{ToolUseID: "call_1", Content: "shot",
				Images: []llm.ImageBlock{{URL: "https://example.com/a.png"}, {MIMEType: "image/png", Data: []byte("png")}}},
		}},
		{Role: llm.RoleTool, Content: []llm.ContentBlock{
			llm.ToolResultBlock{ToolUseID: "call_2"},
		}},
	}
	items, _, err := convertMessages(messages)
	if err != nil {
		t.Fatalf("convertMessages: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	first, err := json.Marshal(items[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(first), `"output":[{"type":"input_text","text":"shot"},{"type":"input_image","image_url":"https://example.com/a.png"},{"type":"input_image","image_url":"data:image/png;base64,cG5n"}]`) {
		t.Errorf("image wire = %s", first)
	}
	second, err := json.Marshal(items[1])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(second), `"output":""`) {
		t.Errorf("empty wire = %s, want \"output\":\"\" preserved", second)
	}
	// The image whitelist applies to tool-result attachments too.
	rejected := []llm.Message{{Role: llm.RoleTool, Content: []llm.ContentBlock{
		llm.ToolResultBlock{ToolUseID: "call_3", Images: []llm.ImageBlock{{URL: "data:garbage"}}},
	}}}
	if _, _, err := convertMessages(rejected); err == nil {
		t.Fatal("accepted garbage image source")
	}
}
