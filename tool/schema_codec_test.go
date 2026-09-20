package tool

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSchemaCodec_LegacyBytes(t *testing.T) {
	vectors := []struct {
		value any
		want  string
	}{
		{ParameterSchema{}, `{"type":"","properties":null}`},
		{NewParameterSchema(), `{"type":"object","properties":{}}`},
		{Property{}, `{"type":"","description":""}`},
		{Property{Type: "string"}, `{"type":"string","description":""}`},
		{Property{Type: "string", Description: "x", Enum: []string{"a"}}, `{"type":"string","description":"x","enum":["a"]}`},
	}
	for _, tt := range vectors {
		got, err := json.Marshal(tt.value)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != tt.want {
			t.Errorf("Marshal(%T) = %s, want %s", tt.value, got, tt.want)
		}
	}
}

func TestSchemaCodec_NormalizationAndPresence(t *testing.T) {
	var schema ParameterSchema
	if err := json.Unmarshal([]byte(`{"description":"","properties":{}}`), &schema); err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"properties":{},"description":""}` {
		t.Fatalf("schema = %s", got)
	}
	var property Property
	if err := json.Unmarshal([]byte(`{"type":["array","null"],"$ref":""}`), &property); err != nil {
		t.Fatal(err)
	}
	got, err = json.Marshal(property)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"type":["null","array"],"$ref":""}` {
		t.Fatalf("property = %s", got)
	}
	if err := json.Unmarshal([]byte(`{"type":"string","description":"stale"}`), &property); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{}`), &property); err != nil {
		t.Fatal(err)
	}
	got, err = json.Marshal(property)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{}` {
		t.Fatalf("reused property = %s", got)
	}
}

func TestSchemaCodec_CarrierCompositionAndBoolean(t *testing.T) {
	input := []byte(`{"type":"object","properties":{"anything":true},"anyOf":[{"$ref":"#/$defs/x"},false],"zeta":1,"alpha":{"x":true}}`)
	var schema ParameterSchema
	if err := json.Unmarshal(input, &schema); err != nil {
		t.Fatal(err)
	}
	if schema.Properties["anything"].BooleanSchema == nil || !*schema.Properties["anything"].BooleanSchema {
		t.Fatal("boolean property was not projected")
	}
	if len(schema.AnyOf) != 2 || schema.AnyOf[1].BooleanSchema == nil || *schema.AnyOf[1].BooleanSchema {
		t.Fatal("composition was not projected")
	}
	got, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"type":"object","properties":{"anything":true},"anyOf":[{"$ref":"#/$defs/x"},false],"alpha":{"x":true},"zeta":1}`
	if string(got) != want {
		t.Fatalf("schema = %s, want %s", got, want)
	}
	if err := json.Unmarshal([]byte(`{"x":1,"x":2}`), &schema); err == nil {
		t.Fatal("duplicate key accepted")
	}
	bad := Property{Type: "string", Keywords: []SchemaKeyword{{Name: "pattern", Value: json.RawMessage(`{"x":1,"x":2}`)}}}
	if _, err := json.Marshal(bad); err == nil {
		t.Fatal("nested duplicate carrier key accepted")
	}
}

func TestSchemaCodec_CyclesAndDAG(t *testing.T) {
	self := map[string]Property{}
	self["self"] = Property{Type: "object", Properties: self}
	mustMarshalError(t, ParameterSchema{Type: "object", Properties: self})
	a, b := map[string]Property{}, map[string]Property{}
	a["b"] = Property{Type: "object", Properties: b}
	b["a"] = Property{Type: "object", Properties: a}
	mustMarshalError(t, ParameterSchema{Type: "object", Properties: a})
	mixed := map[string]Property{}
	child := &Property{Type: "object", Properties: mixed}
	mixed["loop"] = Property{Type: "array", Items: child}
	mustMarshalError(t, ParameterSchema{Type: "object", Properties: mixed})
	ap := map[string]Property{}
	ap["loop"] = Property{Type: "object", AdditionalPropertiesSchema: &Property{Type: "object", Properties: ap}}
	mustMarshalError(t, ParameterSchema{Type: "object", Properties: ap})
	composition := map[string]Property{}
	composition["loop"] = Property{Type: "object", AnyOf: []*Property{{Type: "object", Properties: composition}}}
	mustMarshalError(t, ParameterSchema{Type: "object", Properties: composition})
	shared := &Property{Type: "string", Description: "shared"}
	sharedMap := map[string]Property{"value": {Type: "string"}}
	ok := ParameterSchema{Type: "object", Properties: map[string]Property{"array": {Type: "array", Items: shared}, "union": {Type: "object", AnyOf: []*Property{shared}}, "left": {Type: "object", Properties: sharedMap}, "right": {Type: "object", Properties: sharedMap}}}
	if _, err := json.Marshal(ok); err != nil {
		t.Fatalf("shared DAG rejected: %v", err)
	}
}

func TestSchemaCodec_StructuralErrors(t *testing.T) {
	truth := true
	mustMarshalError(t, Property{Type: "string", BooleanSchema: &truth})
	mustMarshalError(t, Property{Type: "object", AdditionalProperties: &truth, AdditionalPropertiesSchema: &Property{Type: "string"}})
	mustMarshalError(t, Property{Type: "string", Keywords: []SchemaKeyword{{Name: "format", Value: json.RawMessage(`"a"`)}, {Name: "format", Value: json.RawMessage(`"b"`)}}})
}

func mustMarshalError(t *testing.T, value any) {
	t.Helper()
	if _, err := json.Marshal(value); err == nil {
		t.Fatalf("Marshal(%T) succeeded", value)
	} else if !strings.Contains(err.Error(), "schema") && !strings.Contains(err.Error(), "cycle") && !strings.Contains(err.Error(), "additional") && !strings.Contains(err.Error(), "keyword") && !strings.Contains(err.Error(), "boolean") {
		t.Fatalf("unexpected error: %v", err)
	}
}
