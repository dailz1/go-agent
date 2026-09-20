package tool

import (
	"encoding/json"
	"testing"
)

func TestRegistry_SchemaRecursion(t *testing.T) {
	custom := NewParameterSchema()
	custom.Properties["duration"] = Property{Type: "duration", Description: "legacy"}
	if err := NewRegistry().Register(&mockTool{info: ToolInfo{Name: "custom", Parameters: custom}}); err != nil {
		t.Fatalf("legacy custom type rejected: %v", err)
	}
	var decoded ParameterSchema
	if err := json.Unmarshal([]byte(`{"type":"object","properties":{"anything":true,"untyped":{}}}`), &decoded); err != nil {
		t.Fatal(err)
	}
	if err := NewRegistry().Register(&mockTool{info: ToolInfo{Name: "decoded", Parameters: decoded}}); err != nil {
		t.Fatalf("decoded schema rejected: %v", err)
	}
	child := &Property{Type: "string"}
	sharedMap := map[string]Property{"value": {Type: "string"}}
	dag := ParameterSchema{Type: "object", Properties: map[string]Property{"a": {Type: "array", Items: child}, "b": {Type: "object", AnyOf: []*Property{child}}, "left": {Type: "object", Properties: sharedMap}, "right": {Type: "object", Properties: sharedMap}}}
	if err := NewRegistry().Register(&mockTool{info: ToolInfo{Name: "dag", Parameters: dag}}); err != nil {
		t.Fatalf("shared DAG rejected: %v", err)
	}
}

func TestRegistry_SchemaStructuralFailures(t *testing.T) {
	self := map[string]Property{}
	self["self"] = Property{Type: "object", Properties: self}
	left, right := map[string]Property{}, map[string]Property{}
	left["right"] = Property{Type: "object", Properties: right}
	right["left"] = Property{Type: "object", Properties: left}
	mixed := map[string]Property{}
	mixed["loop"] = Property{Type: "array", Items: &Property{Type: "object", Properties: mixed}}
	ap := map[string]Property{}
	ap["loop"] = Property{Type: "object", AdditionalPropertiesSchema: &Property{Type: "object", Properties: ap}}
	composition := map[string]Property{}
	composition["loop"] = Property{Type: "object", AllOf: []*Property{{Type: "object", Properties: composition}}}
	cases := []ParameterSchema{
		{Type: "object", Properties: self},
		{Type: "object", Properties: left},
		{Type: "object", Properties: mixed},
		{Type: "object", Properties: ap},
		{Type: "object", Properties: composition},
		{Type: "object", Properties: map[string]Property{"bad": {Type: "object", AnyOf: []*Property{nil}}}},
		{Type: "object", Properties: map[string]Property{"bad": {Type: "string", Keywords: []SchemaKeyword{{Name: "type", Value: json.RawMessage(`"string"`)}}}}},
	}
	for i, schema := range cases {
		if err := NewRegistry().Register(&mockTool{info: ToolInfo{Name: string(rune('a' + i)), Parameters: schema}}); err == nil {
			t.Fatalf("case %d registered", i)
		}
	}
}

func TestRegistry_CarriedPropertiesSkipRequiredSubset(t *testing.T) {
	for _, input := range []string{
		`{"properties":[],"required":["missing"]}`,
		`{"properties":{"nested":{"properties":null,"required":["missing"]}}}`,
	} {
		var schema ParameterSchema
		if err := json.Unmarshal([]byte(input), &schema); err != nil {
			t.Fatal(err)
		}
		if err := NewRegistry().Register(&mockTool{info: ToolInfo{Name: input, Parameters: schema}}); err != nil {
			t.Fatalf("carried properties rejected: %v", err)
		}
	}
}

func TestRegistry_NestedRequiredIsValidated(t *testing.T) {
	schema := ParameterSchema{Type: "object", Properties: map[string]Property{
		"nested": {Type: "object", Properties: map[string]Property{}, Required: []string{"missing"}},
	}}
	if err := NewRegistry().Register(&mockTool{info: ToolInfo{Name: "nested", Parameters: schema}}); err == nil {
		t.Fatal("missing nested required property registered")
	}
}
