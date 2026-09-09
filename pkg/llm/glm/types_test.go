package glm

import (
	"encoding/json"
	"strings"
	"testing"
)

func ptr(f float64) *float64 { return &f }

func TestChatRequest_TopP(t *testing.T) {
	req := chatRequest{
		Model:    "glm-4",
		Messages: []chatMessage{{Role: "user", Content: "hi"}},
		TopP:     ptr(0.9),
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"top_p"`) {
		t.Errorf("expected top_p in JSON, got %s", data)
	}
	if !strings.Contains(string(data), `0.9`) {
		t.Errorf("expected 0.9 in JSON, got %s", data)
	}
}

func TestChatRequest_ToolStream(t *testing.T) {
	req := chatRequest{
		Model:       "glm-4",
		Messages:    []chatMessage{{Role: "user", Content: "hi"}},
		ToolStream:  true,
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"tool_stream"`) {
		t.Errorf("expected tool_stream in JSON, got %s", data)
	}
	if !strings.Contains(string(data), `true`) {
		t.Errorf("expected true in JSON, got %s", data)
	}
}

func TestChatRequest_ThinkingEnabled(t *testing.T) {
	req := chatRequest{
		Model:    "glm-4",
		Messages: []chatMessage{{Role: "user", Content: "hi"}},
		Thinking: &thinkingConfig{
			Type:          "enabled",
			ClearThinking: true,
		},
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	raw := string(data)
	if !strings.Contains(raw, `"thinking"`) {
		t.Errorf("expected thinking in JSON, got %s", raw)
	}
	if !strings.Contains(raw, `"clear_thinking"`) {
		t.Errorf("expected clear_thinking in JSON, got %s", raw)
	}
	if !strings.Contains(raw, `"enabled"`) {
		t.Errorf("expected enabled type in JSON, got %s", raw)
	}
}

func TestChatRequest_StopWords(t *testing.T) {
	req := chatRequest{
		Model:    "glm-4",
		Messages: []chatMessage{{Role: "user", Content: "hi"}},
		Stop:     []string{"stop"},
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	raw := string(data)
	if !strings.Contains(raw, `"stop"`) {
		t.Errorf("expected stop in JSON, got %s", raw)
	}
	if !strings.Contains(raw, `["stop"]`) {
		t.Errorf("expected [\"stop\"] in JSON, got %s", raw)
	}
}

func TestChatRequest_RequestID(t *testing.T) {
	req := chatRequest{
		Model:     "glm-4",
		Messages:  []chatMessage{{Role: "user", Content: "hi"}},
		RequestID: "req-123",
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	raw := string(data)
	if !strings.Contains(raw, `"request_id"`) {
		t.Errorf("expected request_id in JSON, got %s", raw)
	}
	if !strings.Contains(raw, `"req-123"`) {
		t.Errorf("expected req-123 in JSON, got %s", raw)
	}
}

func TestChatRequest_UserID(t *testing.T) {
	req := chatRequest{
		Model:    "glm-4",
		Messages: []chatMessage{{Role: "user", Content: "hi"}},
		UserID:   "user-456",
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	raw := string(data)
	if !strings.Contains(raw, `"user_id"`) {
		t.Errorf("expected user_id in JSON, got %s", raw)
	}
	if !strings.Contains(raw, `"user-456"`) {
		t.Errorf("expected user-456 in JSON, got %s", raw)
	}
}

func TestChatRequest_TemperatureZeroNotOmitted(t *testing.T) {
	zero := 0.0
	req := chatRequest{
		Model:       "glm-4",
		Messages:    []chatMessage{{Role: "user", Content: "hi"}},
		Temperature: &zero,
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	raw := string(data)
	if !strings.Contains(raw, `"temperature"`) {
		t.Errorf("expected temperature field in JSON when set to 0, got %s", raw)
	}
	if !strings.Contains(raw, `"temperature":0`) {
		t.Errorf("expected temperature:0 in JSON, got %s", raw)
	}
}

func TestChatRequest_TemperatureNilOmitted(t *testing.T) {
	req := chatRequest{
		Model:    "glm-4",
		Messages: []chatMessage{{Role: "user", Content: "hi"}},
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), `"temperature"`) {
		t.Errorf("expected temperature to be omitted when nil, got %s", data)
	}
}

func TestChatRequest_TopPZeroNotOmitted(t *testing.T) {
	zero := 0.0
	req := chatRequest{
		Model:    "glm-4",
		Messages: []chatMessage{{Role: "user", Content: "hi"}},
		TopP:     &zero,
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	raw := string(data)
	if !strings.Contains(raw, `"top_p"`) {
		t.Errorf("expected top_p field in JSON when set to 0, got %s", raw)
	}
	if !strings.Contains(raw, `"top_p":0`) {
		t.Errorf("expected top_p:0 in JSON, got %s", raw)
	}
}

func TestChatRequest_TopPNilOmitted(t *testing.T) {
	req := chatRequest{
		Model:    "glm-4",
		Messages: []chatMessage{{Role: "user", Content: "hi"}},
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), `"top_p"`) {
		t.Errorf("expected top_p to be omitted when nil, got %s", data)
	}
}

func TestChatResponse_WebSearchField(t *testing.T) {
	raw := `{
		"id": "chat-1",
		"model": "glm-4",
		"request_id": "req-1",
		"created": 1234567890,
		"choices": [],
		"web_search": [{"title": "result", "link": "https://example.com"}],
		"content_filter": [{"type": "violence", "level": "safe"}]
	}`

	var resp chatResponse
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.WebSearch) != 1 {
		t.Errorf("expected 1 web_search result, got %d", len(resp.WebSearch))
	}
	if resp.Created != 1234567890 {
		t.Errorf("expected created=1234567890, got %d", resp.Created)
	}
}

func TestChatResponse_ContentFilterField(t *testing.T) {
	raw := `{
		"id": "chat-1",
		"model": "glm-4",
		"request_id": "req-1",
		"created": 1234567890,
		"choices": [],
		"content_filter": [{"type": "violence", "level": "safe"}]
	}`

	var resp chatResponse
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.ContentFilter) != 1 {
		t.Errorf("expected 1 content_filter result, got %d", len(resp.ContentFilter))
	}
}

func TestChatResponse_ReasoningContent(t *testing.T) {
	content := "Let me think about this..."
	reasoning := "Step 1: analyze the problem"
	raw := `{
		"id": "chat-1",
		"model": "glm-4",
		"request_id": "req-1",
		"created": 1234567890,
		"choices": [{
			"index": 0,
			"message": {
				"role": "assistant",
				"content": "` + content + `",
				"reasoning_content": "` + reasoning + `"
			},
			"finish_reason": "stop"
		}]
	}`

	var resp chatResponse
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("expected 1 choice, got %d", len(resp.Choices))
	}
	msg := resp.Choices[0].Message
	if msg.ReasoningContent == nil || *msg.ReasoningContent != reasoning {
		t.Errorf("expected reasoning_content=%q, got %v", reasoning, msg.ReasoningContent)
	}
	if msg.Content == nil || *msg.Content != content {
		t.Errorf("expected content=%q, got %v", content, msg.Content)
	}
}
