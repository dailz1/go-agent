package tool

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNewErrorResult(t *testing.T) {
	r := NewErrorResult("something failed: %s", "detail")

	if !r.IsError() {
		t.Error("IsError() = false, want true")
	}
	if r.Status != ResultError {
		t.Errorf("Status = %q, want %q", r.Status, ResultError)
	}
	want := "something failed: detail"
	if r.Content != want {
		t.Errorf("Content = %q, want %q", r.Content, want)
	}
}

func TestNewTextResult(t *testing.T) {
	r := NewTextResult("hello world")

	if r.IsError() {
		t.Error("IsError() = true, want false")
	}
	if r.Status != "" {
		t.Errorf("Status = %q, want empty string", r.Status)
	}
	if r.Content != "hello world" {
		t.Errorf("Content = %q, want %q", r.Content, "hello world")
	}
}

func TestNewDataResult(t *testing.T) {
	data := map[string]int{"x": 1, "y": 2}
	r, err := NewDataResult("result", data)
	if err != nil {
		t.Fatalf("NewDataResult returned error: %v", err)
	}

	if r.Content != "result" {
		t.Errorf("Content = %q, want %q", r.Content, "result")
	}

	var parsed map[string]int
	if err := json.Unmarshal(r.Data, &parsed); err != nil {
		t.Fatalf("failed to unmarshal Data: %v", err)
	}
	if parsed["x"] != 1 || parsed["y"] != 2 {
		t.Errorf("Data = %s, want {\"x\":1,\"y\":2}", string(r.Data))
	}
}

func TestNewDataResult_Error(t *testing.T) {
	ch := make(chan int)
	_, err := NewDataResult("bad", ch)
	if err == nil {
		t.Fatal("expected error for unmarshallable value, got nil")
	}
	if !strings.Contains(err.Error(), "marshal") {
		t.Errorf("error message = %q, want mention of marshal", err.Error())
	}
}

func TestNewParameterSchema(t *testing.T) {
	s := NewParameterSchema()

	if s.Type != "object" {
		t.Errorf("Type = %q, want %q", s.Type, "object")
	}
	if s.Properties == nil {
		t.Error("Properties is nil, want non-nil")
	}
	if len(s.Properties) != 0 {
		t.Errorf("Properties has %d entries, want 0", len(s.Properties))
	}
}

func TestParam(t *testing.T) {
	p := Param("string", "the name of the thing")

	if p.Type != "string" {
		t.Errorf("Type = %q, want %q", p.Type, "string")
	}
	if p.Description != "the name of the thing" {
		t.Errorf("Description = %q, want %q", p.Description, "the name of the thing")
	}
}

func TestParamEnum(t *testing.T) {
	p := ParamEnum("choose one", "a", "b", "c")

	if p.Type != "string" {
		t.Errorf("Type = %q, want %q", p.Type, "string")
	}
	if p.Description != "choose one" {
		t.Errorf("Description = %q, want %q", p.Description, "choose one")
	}
	if len(p.Enum) != 3 {
		t.Fatalf("len(Enum) = %d, want 3", len(p.Enum))
	}
	want := []string{"a", "b", "c"}
	for i, v := range want {
		if p.Enum[i] != v {
			t.Errorf("Enum[%d] = %q, want %q", i, p.Enum[i], v)
		}
	}
}

func TestNewTextResult_EmptyContent(t *testing.T) {
	r := NewTextResult("")

	if r.IsError() {
		t.Error("IsError() = true, want false")
	}
	if r.Content != "" {
		t.Errorf("Content = %q, want empty string", r.Content)
	}
	if r.Data != nil {
		t.Error("Data should be nil for text result")
	}
}

func TestNewErrorResult_EmptyFormat(t *testing.T) {
	r := NewErrorResult("")
	if !r.IsError() {
		t.Error("IsError() = false, want true")
	}
	if r.Content != "" {
		t.Errorf("Content = %q, want empty string", r.Content)
	}
}

func TestNewDataResult_NilData(t *testing.T) {
	r, err := NewDataResult("nil payload", nil)
	if err != nil {
		t.Fatalf("NewDataResult with nil returned error: %v", err)
	}
	if r.Content != "nil payload" {
		t.Errorf("Content = %q, want %q", r.Content, "nil payload")
	}
	if string(r.Data) != "null" {
		t.Errorf("Data = %s, want \"null\"", string(r.Data))
	}
}

func TestToolResult_IsError_StatusValues(t *testing.T) {
	tests := []struct {
		name    string
		status  ResultStatus
		wantErr bool
	}{
		{"empty string is success", "", false},
		{"error status", "error", true},
		{"success literal", "success", false},
		{"warning status", "warning", false},
		{"random status", "something", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &ToolResult{Status: tt.status}
			if got := r.IsError(); got != tt.wantErr {
				t.Errorf("IsError() = %v, want %v for status %q", got, tt.wantErr, tt.status)
			}
		})
	}
}

func TestNewParameterSchema_AddProperty(t *testing.T) {
	s := NewParameterSchema()
	s.Properties["name"] = Param("string", "the name")
	s.Properties["count"] = Param("integer", "item count")
	s.Required = []string{"name"}

	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("failed to marshal schema: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(b, &parsed); err != nil {
		t.Fatalf("failed to unmarshal schema: %v", err)
	}

	if parsed["type"] != "object" {
		t.Errorf("type = %v, want \"object\"", parsed["type"])
	}

	props, ok := parsed["properties"].(map[string]any)
	if !ok {
		t.Fatal("properties is not a map")
	}
	if len(props) != 2 {
		t.Errorf("properties has %d entries, want 2", len(props))
	}

	nameProp, ok := props["name"].(map[string]any)
	if !ok {
		t.Fatal("name property is not a map")
	}
	if nameProp["type"] != "string" {
		t.Errorf("name.type = %v, want \"string\"", nameProp["type"])
	}

	req, ok := parsed["required"].([]any)
	if !ok {
		t.Fatal("required is not a slice")
	}
	if len(req) != 1 || req[0] != "name" {
		t.Errorf("required = %v, want [\"name\"]", req)
	}
}

func TestParamEnum_Empty(t *testing.T) {
	p := ParamEnum("pick something")

	if p.Type != "string" {
		t.Errorf("Type = %q, want %q", p.Type, "string")
	}
	if p.Description != "pick something" {
		t.Errorf("Description = %q, want %q", p.Description, "pick something")
	}
	if len(p.Enum) != 0 {
		t.Errorf("len(Enum) = %d, want 0", len(p.Enum))
	}

	// Verify omitempty: Enum should not appear in JSON output.
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("failed to marshal Property: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(b, &parsed); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if _, exists := parsed["enum"]; exists {
		t.Error("enum key present in JSON, want omitted for empty slice")
	}
}

func TestToolResult_JSONRoundTrip(t *testing.T) {
	original := &ToolResult{
		Content: "some data",
		Data:    json.RawMessage(`{"key":"value","num":42}`),
		Status:  ResultSuccess,
	}

	b, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}

	var restored ToolResult
	if err := json.Unmarshal(b, &restored); err != nil {
		t.Fatalf("Unmarshal returned error: %v", err)
	}

	if restored.Content != original.Content {
		t.Errorf("Content = %q, want %q", restored.Content, original.Content)
	}
	if string(restored.Data) != string(original.Data) {
		t.Errorf("Data = %s, want %s", string(restored.Data), string(original.Data))
	}
	if restored.Status != original.Status {
		t.Errorf("Status = %q, want %q", restored.Status, original.Status)
	}
	if restored.IsError() {
		t.Error("IsError() = true for success result, want false")
	}
}
