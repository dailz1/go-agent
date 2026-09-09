package openai

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestChatRequest_TemperatureZeroNotOmitted(t *testing.T) {
	zero := 0.0
	req := chatRequest{
		Model:       "gpt-4o",
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
		Model:    "gpt-4o",
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
