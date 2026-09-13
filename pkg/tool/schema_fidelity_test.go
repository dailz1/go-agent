package tool

import (
	"encoding/json"
	"testing"
)

func TestSchemaCodec_CompleteLegacyVector(t *testing.T) {
	schema := ParameterSchema{
		Type: "object",
		Properties: map[string]Property{
			"mode": {Type: "string", Description: "mode", Enum: []string{"fast", "safe"}},
			"name": {Type: "string", Description: "name"},
		},
		Required: []string{"name"},
	}
	assertSchemaJSON(t, schema, `{"type":"object","properties":{"mode":{"type":"string","description":"mode","enum":["fast","safe"]},"name":{"type":"string","description":"name"}},"required":["name"]}`)
}

func TestSchemaCodec_RootNormalizationAndFallback(t *testing.T) {
	cases := []struct{ input, want string }{
		{`{}`, `{"properties":{}}`},
		{`{"properties":{}}`, `{"properties":{}}`},
		{`{"properties":null}`, `{"properties":null}`},
		{`{"type":["object","null"],"properties":{}}`, `{"properties":{},"type":["object","null"]}`},
		{`{"type":null}`, `{"properties":{},"type":null}`},
		{`{"type":[null,"object"]}`, `{"properties":{},"type":[null,"object"]}`},
		{`{"properties":[]}`, `{"properties":[]}`},
		{`{"required":null}`, `{"properties":{},"required":null}`},
		{`{"required":[null]}`, `{"properties":{},"required":[null]}`},
		{`{"description":null}`, `{"properties":{},"description":null}`},
		{`{"additionalProperties":null}`, `{"properties":{},"additionalProperties":null}`},
		{`{"anyOf":[]}`, `{"properties":{},"anyOf":[]}`},
		{`{"oneOf":null}`, `{"properties":{},"oneOf":null}`},
		{`{"allOf":{}}`, `{"properties":{},"allOf":{}}`},
		{`{"$ref":null}`, `{"properties":{},"$ref":null}`},
	}
	for _, tt := range cases {
		var schema ParameterSchema
		if err := json.Unmarshal([]byte(tt.input), &schema); err != nil {
			t.Fatalf("Unmarshal(%s): %v", tt.input, err)
		}
		assertSchemaJSON(t, schema, tt.want)
	}
}

func TestSchemaCodec_PropertyFallbackAndNormalization(t *testing.T) {
	cases := []struct{ input, want string }{
		{`{"type":null}`, `{"type":null}`},
		{`{"description":null}`, `{"description":null}`},
		{`{"enum":null}`, `{"enum":null}`},
		{`{"enum":[]}`, `{"enum":[]}`},
		{`{"enum":[null]}`, `{"enum":[null]}`},
		{`{"properties":null}`, `{"properties":null}`},
		{`{"properties":{}}`, `{}`},
		{`{"required":null}`, `{"required":null}`},
		{`{"items":null}`, `{"items":null}`},
		{`{"additionalProperties":null}`, `{"additionalProperties":null}`},
		{`{"anyOf":null}`, `{"anyOf":null}`},
		{`{"oneOf":[]}`, `{"oneOf":[]}`},
		{`{"allOf":{}}`, `{"allOf":{}}`},
		{`{"$ref":null}`, `{"$ref":null}`},
	}
	for _, tt := range cases {
		var property Property
		if err := json.Unmarshal([]byte(tt.input), &property); err != nil {
			t.Fatalf("Unmarshal(%s): %v", tt.input, err)
		}
		assertSchemaJSON(t, property, tt.want)
	}
}

func TestSchemaCodec_AdditionalPropertiesAndLargeNumber(t *testing.T) {
	assertSchemaJSON(t, Property{Type: "object", Properties: map[string]Property{}}, `{"type":"object","description":""}`)
	for _, input := range []string{
		`{"type":"object","additionalProperties":true}`,
		`{"type":"object","additionalProperties":false}`,
		`{"type":"object","additionalProperties":{"type":"string"}}`,
	} {
		var property Property
		if err := json.Unmarshal([]byte(input), &property); err != nil {
			t.Fatal(err)
		}
		assertSchemaJSON(t, property, input)
	}
	var schema ParameterSchema
	if err := json.Unmarshal([]byte(`{"minimum":1e1000}`), &schema); err != nil {
		t.Fatal(err)
	}
	assertSchemaJSON(t, schema, `{"properties":{},"minimum":1e1000}`)
}

func TestSchemaCodec_BooleanRejectsExplicitEmptyRef(t *testing.T) {
	var property Property
	if err := json.Unmarshal([]byte(`{"$ref":""}`), &property); err != nil {
		t.Fatal(err)
	}
	assertSchemaJSON(t, property, `{"$ref":""}`)
	truth := true
	property.BooleanSchema = &truth
	if _, err := json.Marshal(property); err == nil {
		t.Fatal("BooleanSchema dropped an explicit empty $ref")
	}
}

func assertSchemaJSON(t *testing.T, value any, want string) {
	t.Helper()
	got, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("Marshal(%T) = %s, want %s", value, got, want)
	}
}
