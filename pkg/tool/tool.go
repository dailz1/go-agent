package tool

import (
	"context"
	"encoding/json"
	"fmt"
)

// Tool is the core interface that every agent tool must implement.
//
// A Tool serves two purposes:
//  1. Describe itself to the LLM (via Info) so the model knows when and how to call it.
//  2. Execute the actual work (via Execute) when the LLM decides to invoke it.
type Tool interface {
	// Info returns metadata describing the tool — its name, what it does,
	// and what parameters it accepts. This maps directly to the function
	// calling schema consumed by LLM providers (OpenAI, Anthropic, etc.).
	Info() ToolInfo

	// Execute runs the tool with the arguments produced by the LLM.
	//
	// args is raw JSON — the concrete tool implementation is responsible for
	// unmarshalling it into the appropriate struct.
	//
	// Return a ToolResult with IsError=true for "soft" failures (bad input,
	// resource not found, etc.) so the agent loop can feed the error back to
	// the LLM. Return a Go error only for system-level failures (network
	// down, panic, etc.) that should interrupt the loop.
	Execute(ctx context.Context, args json.RawMessage) (*ToolResult, error)
}

// ToolInfo describes a tool's identity and parameter schema.
type ToolInfo struct {
	Name             string          `json:"name"`
	Description      string          `json:"description"`
	Parameters       ParameterSchema `json:"parameters"`
	RequiresApproval bool            `json:"requires_approval,omitempty"`
}

// SchemaKeyword carries one JSON Schema keyword that has no typed representation.
// Value must contain exactly one valid JSON value.
type SchemaKeyword struct {
	Name  string
	Value json.RawMessage
}

// ParameterSchema is a JSON Schema object that describes tool parameters.
// Its custom JSON codec preserves typed recursive fields and carried keywords.
type ParameterSchema struct {
	Type                       string
	Properties                 map[string]Property
	Required                   []string
	Description                string
	AdditionalProperties       *bool
	AdditionalPropertiesSchema *Property
	AnyOf                      []*Property
	OneOf                      []*Property
	AllOf                      []*Property
	Ref                        string
	Keywords                   []SchemaKeyword

	typeAbsent, propertiesInactive, descriptionPresent, refPresent bool
}

// Property describes a JSON Schema node within a ParameterSchema.
type Property struct {
	Type                       string
	Description                string
	Enum                       []string
	Nullable                   bool
	Properties                 map[string]Property
	Required                   []string
	Items                      *Property
	AdditionalProperties       *bool
	AdditionalPropertiesSchema *Property
	AnyOf                      []*Property
	OneOf                      []*Property
	AllOf                      []*Property
	Ref                        string
	Keywords                   []SchemaKeyword
	BooleanSchema              *bool

	typeAbsent, propertiesInactive, descriptionAbsent, refPresent, decoded bool
}

type ResultStatus string

const (
	ResultSuccess ResultStatus = ""
	ResultError   ResultStatus = "error"
)

type ToolResult struct {
	Content string          `json:"content"`
	Data    json.RawMessage `json:"data,omitempty"`
	Status  ResultStatus    `json:"status,omitempty"`
}

func (r *ToolResult) IsError() bool { return r.Status == ResultError }

func NewErrorResult(format string, args ...any) *ToolResult {
	return &ToolResult{
		Content: fmt.Sprintf(format, args...),
		Status:  ResultError,
	}
}

// NewTextResult is a convenience constructor for plain text results.
func NewTextResult(content string) *ToolResult {
	return &ToolResult{
		Content: content,
	}
}

// NewDataResult is a convenience constructor for results with structured data.
func NewDataResult(content string, data any) (*ToolResult, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("marshal tool result data: %w", err)
	}
	return &ToolResult{
		Content: content,
		Data:    raw,
	}, nil
}

// --- Schema builder helpers ---

// NewParameterSchema creates a ParameterSchema with type "object" and initializes
// the Properties map.
func NewParameterSchema() ParameterSchema {
	return ParameterSchema{
		Type:       "object",
		Properties: make(map[string]Property),
	}
}

// Param is a shorthand to define a required parameter Property.
func Param(typ string, description string) Property {
	return Property{Type: typ, Description: description}
}

// ParamEnum is a shorthand to define a parameter with a fixed set of values.
func ParamEnum(description string, values ...string) Property {
	return Property{Type: "string", Description: description, Enum: values}
}
