package tool_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/dailz1/go-agent/tool"
)

type externalTool struct{ info tool.ToolInfo }

func (t externalTool) Info() tool.ToolInfo { return t.info }
func (externalTool) Execute(context.Context, json.RawMessage) (*tool.ToolResult, error) {
	return tool.NewTextResult("ok"), nil
}

func TestSchemaExternalKeyedAPI(t *testing.T) {
	truth, falsity := true, false
	stringSchema := &tool.Property{Type: "string"}
	cases := []struct {
		name  string
		value tool.Property
		want  string
	}{
		{"boolean", tool.Property{BooleanSchema: &truth, Ref: ""}, `true`},
		{"ref", tool.Property{Ref: "#/$defs/value"}, `{"$ref":"#/$defs/value"}`},
		{"items", tool.Property{Items: stringSchema}, `{"items":{"type":"string","description":""}}`},
		{"additional properties bool", tool.Property{AdditionalProperties: &falsity}, `{"additionalProperties":false}`},
		{"additional properties schema", tool.Property{AdditionalPropertiesSchema: stringSchema}, `{"additionalProperties":{"type":"string","description":""}}`},
		{"composition", tool.Property{AnyOf: []*tool.Property{stringSchema}}, `{"anyOf":[{"type":"string","description":""}]}`},
		{"nullable", tool.Property{Type: "string", Nullable: true}, `{"type":["null","string"]}`},
		{"carrier", tool.Property{Keywords: []tool.SchemaKeyword{{Name: "format", Value: json.RawMessage(`"uuid"`)}}}, `{"format":"uuid"}`},
	}
	schema := tool.NewParameterSchema()
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got, err := json.Marshal(tt.value)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Fatalf("Marshal = %s, want %s", got, tt.want)
			}
		})
		schema.Properties[tt.name] = tt.value
	}
	info := tool.ToolInfo{Name: "external", Parameters: schema}
	if err := tool.NewRegistry().Register(externalTool{info: info}); err != nil {
		t.Fatal(err)
	}
	if _, err := json.Marshal(info.Parameters); err != nil {
		t.Fatal(err)
	}
}
