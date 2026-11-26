package codemode

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func renderOne(t *testing.T, schema string) (string, []string, error) {
	t.Helper()
	desc := ToolDescriptor{Name: "t", InputSchema: json.RawMessage(schema)}
	decl, _, diags, err := renderToolDeclaration(desc)
	return decl, diags, err
}

func mustContain(t *testing.T, got, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Fatalf("declaration %q does not contain %q", got, want)
	}
}

func TestRenderPrimitives(t *testing.T) {
	cases := []struct {
		name   string
		schema string
		want   string
	}{
		{"string", `{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`, `tools["t"](args: { "a": string; }): Promise<ToolValue>`},
		{"number", `{"type":"object","properties":{"a":{"type":"number"}},"required":["a"]}`, `"a": number;`},
		{"boolean", `{"type":"object","properties":{"a":{"type":"boolean"}},"required":["a"]}`, `"a": boolean;`},
		{"null type", `{"type":"object","properties":{"a":{"type":"null"}},"required":["a"]}`, `"a": null;`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decl, _, err := renderOne(t, tc.schema)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			mustContain(t, decl, tc.want)
		})
	}
}

func TestRenderIntegerNotes(t *testing.T) {
	desc := ToolDescriptor{Name: "t", InputSchema: json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}`)}
	decl, notes, _, err := renderToolDeclaration(desc)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	mustContain(t, decl, `"n": number;`)
	if len(notes) == 0 {
		t.Fatalf("integer must produce a note")
	}
	for _, n := range notes {
		if !strings.Contains(n, "integer") {
			t.Fatalf("note %q does not mention integer", n)
		}
	}
}

func TestRenderObjectShape(t *testing.T) {
	decl, _, err := renderOne(t, `{"type":"object","properties":{"z":{"type":"string"},"a":{"type":"number"}},"required":["z"],"additionalProperties":false}`)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	// keys sorted: a before z; z required, a optional
	mustContain(t, decl, `{ "a"?: number; "z": string; }`)
	for _, d := range _diagsEmpty() {
		_ = d
	}
}

func _diagsEmpty() []string { return nil }

func TestRenderArrays(t *testing.T) {
	decl, _, err := renderOne(t, `{"type":"object","properties":{"a":{"type":"array","items":{"type":"string"}},"b":{"type":"array"}},"required":["a","b"]}`)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	mustContain(t, decl, `"a": string[];`)
	mustContain(t, decl, `"b": any[];`)
}

func TestRenderEnumNullableConst(t *testing.T) {
	decl, _, err := renderOne(t, `{"type":"object","properties":{"e":{"enum":["a","b"]},"n":{"type":"string","nullable":true},"c":{"const":"fixed"}},"required":["e","n","c"]}`)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	mustContain(t, decl, `"e": "a" | "b";`)
	mustContain(t, decl, `"n": string | null;`)
	mustContain(t, decl, `"c": "fixed";`)
}

func TestRenderCompositions(t *testing.T) {
	decl, diags, err := renderOne(t, `{"type":"object","properties":{
		"u":{"anyOf":[{"type":"string"},{"type":"number"}]},
		"o":{"oneOf":[{"type":"string"},{"type":"number"}]},
		"x":{"allOf":[{"type":"object","properties":{"a":{"type":"string"}}},{"type":"object","properties":{"b":{"type":"string"}}}]}
	},"required":["u","o","x"]}`)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	mustContain(t, decl, `"u": (string | number);`)
	mustContain(t, decl, `"o": (string | number);`)
	mustContain(t, decl, `"x": ({ "a"?: string; } & { "b"?: string; });`)
	found := false
	for _, d := range diags {
		if strings.Contains(d, "oneOf exclusivity") {
			found = true
		}
	}
	if !found {
		t.Fatalf("oneOf must produce an exclusivity diagnostic, got %v", diags)
	}
}

func TestRenderLocalRefAndCycle(t *testing.T) {
	decl, diags, err := renderOne(t, `{"type":"object","$defs":{"name":{"type":"string"}},"properties":{"a":{"$ref":"#/$defs/name"},"loop":{"$ref":"#/properties/loop"}},"required":["a","loop"]}`)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	mustContain(t, decl, `"a": string;`)
	mustContain(t, decl, `"loop": any;`)
	cycle := false
	for _, d := range diags {
		if strings.Contains(d, "cycle") {
			cycle = true
		}
	}
	if !cycle {
		t.Fatalf("cycle must produce a diagnostic, got %v", diags)
	}
}

func TestRenderExternalRefNeverFetched(t *testing.T) {
	_, diags, err := renderOne(t, `{"type":"object","properties":{"a":{"$ref":"https://evil.example/schema.json"}},"required":["a"]}`)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	found := false
	for _, d := range diags {
		if strings.Contains(d, "external $ref") && strings.Contains(d, "never fetched") {
			found = true
		}
	}
	if !found {
		t.Fatalf("external ref must produce a no-I/O diagnostic, got %v", diags)
	}
}

func TestRenderUnknownVocabAnyDiag(t *testing.T) {
	_, diags, err := renderOne(t, `{"type":"object","properties":{"a":{"weirdKeyword":{"x":1}}},"required":["a"]}`)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if len(diags) == 0 {
		t.Fatalf("unknown vocabulary must produce a diagnostic")
	}
}

func TestRenderBooleanSchemas(t *testing.T) {
	decl, _, err := renderOne(t, `{"type":"object","properties":{"a":true,"b":false},"required":["a","b"]}`)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	mustContain(t, decl, `"a": any;`)
	mustContain(t, decl, `"b": never;`)
}

func TestRenderAdditionalPropertiesSchema(t *testing.T) {
	decl, _, err := renderOne(t, `{"type":"object","properties":{"a":{"type":"string"}},"additionalProperties":{"type":"number"}}`)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	mustContain(t, decl, `[k: string]: number;`)
}

func TestRootExcludingObjectFailsClosed(t *testing.T) {
	for _, schema := range []string{
		`{"type":"string"}`,
		`{"type":["string","number"]}`,
		`{"enum":["a",1]}`,
		`{"const":"x"}`,
	} {
		_, _, err := renderOne(t, schema)
		if err == nil {
			t.Fatalf("schema %s must hard-fail generation", schema)
		}
		var se *schemaError
		if !asSchemaError(err, &se) {
			t.Fatalf("schema %s: expected *schemaError, got %T", schema, err)
		}
	}
	for _, schema := range []string{
		`{"type":"object"}`,
		`{"type":["object","string"]}`,
		`{"anyOf":[{"type":"string"},{"type":"object","properties":{}}]}`,
		`{"weird":{"a":1}}`,
		``,
	} {
		if _, _, err := renderOne(t, schema); err != nil {
			t.Fatalf("schema %s must not fail: %v", schema, err)
		}
	}
}

func asSchemaError(err error, target **schemaError) bool {
	if se, ok := err.(*schemaError); ok {
		*target = se
		return true
	}
	return false
}

func TestRenderDeterministic(t *testing.T) {
	schema := `{"type":"object","properties":{"z":{"type":"string"},"a":{"type":"number"},"m":{"type":"array","items":{"type":"integer"}}},"required":["z"]}`
	first, diags1, err := renderOne(t, schema)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for range 10 {
		again, diags2, err := renderOne(t, schema)
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		if again != first || strings.Join(diags2, "|") != strings.Join(diags1, "|") {
			t.Fatalf("render is not deterministic:\n%s\nvs\n%s", first, again)
		}
	}
}

func TestQuoteJSNameEscapes(t *testing.T) {
	decl, _, err := renderOne(t, `{"type":"object"}`)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(decl, `tools["t"]`) {
		t.Fatalf("unexpected declaration %q", decl)
	}
	desc := ToolDescriptor{Name: "a-b with \"quote\"", InputSchema: json.RawMessage(`{"type":"object"}`)}
	decl2, _, _, err := renderToolDeclaration(desc)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(decl2, `tools["a-b with \"quote\""]`) {
		t.Fatalf("name not JSON-escaped: %q", decl2)
	}
}

func TestRefDepthDegradation(t *testing.T) {
	// d1 -> d2 -> ... -> d25 -> string; resolving 25 chained local refs
	// exceeds maxRefDepth so the subtree degrades to any with a diagnostic.
	var defs strings.Builder
	for i := 1; i <= 25; i++ {
		if i > 1 {
			defs.WriteString(",")
		}
		if i == 25 {
			defs.WriteString(`"d25":{"type":"string"}`)
		} else {
			defs.WriteString(fmt.Sprintf(`"d%d":{"$ref":"#/$defs/d%d"}`, i, i+1))
		}
	}
	schema := fmt.Sprintf(`{"type":"object","$defs":{%s},"properties":{"deep":{"$ref":"#/$defs/d1"}},"required":["deep"]}`, defs.String())
	decl, diags, err := renderOne(t, schema)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(decl, `"deep": any;`) {
		t.Fatalf("deep ref chain must degrade to any, got %q", decl)
	}
	found := false
	for _, d := range diags {
		if strings.Contains(d, "nesting exceeds") {
			found = true
		}
	}
	if !found {
		t.Fatalf("depth overflow must produce a diagnostic, got %v", diags)
	}
}
