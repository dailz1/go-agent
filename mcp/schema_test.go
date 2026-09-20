package mcpbridge

import (
	"context"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/tool"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type inferredInput struct {
	Names  *[]string          `json:"names"`
	Any    any                `json:"any"`
	Small  int8               `json:"small"`
	Fixed  [2]string          `json:"fixed"`
	Map    map[string]float64 `json:"map"`
	Nested struct {
		Value string `json:"value"`
	} `json:"nested"`
}

func TestSDKSchemaProjectionPreservesTypedAndCarriedFields(t *testing.T) {
	bridge, registry := connectTestBridge(t, func(s *sdk.Server) {
		sdk.AddTool(s, &sdk.Tool{Name: "generic"}, func(context.Context, *sdk.CallToolRequest, inferredInput) (*sdk.CallToolResult, any, error) {
			return &sdk.CallToolResult{}, nil, nil
		})
		sdk.AddTool(s, &sdk.Tool{Name: "topmap"}, func(context.Context, *sdk.CallToolRequest, map[string]float64) (*sdk.CallToolResult, any, error) {
			return &sdk.CallToolResult{}, nil, nil
		})
		s.AddTool(&sdk.Tool{Name: "ref", InputSchema: map[string]any{
			"type": "object", "properties": map[string]any{"x": map[string]any{"$ref": "#/$defs/x", "z-custom": 1, "a-custom": true}},
			"$defs": map[string]any{"x": map[string]any{"type": "string"}}, "allOf": []any{map[string]any{"type": "object"}},
		}}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return &sdk.CallToolResult{}, nil
		})
	})
	defer func() { _ = bridge.Close() }()

	generic, ok := registry.Get("remote__generic")
	if !ok {
		t.Fatal("generic tool was not registered")
	}
	params := generic.Info().Parameters
	names := params.Properties["names"]
	if !names.Nullable || names.Items == nil || names.Items.Type != "string" {
		t.Fatalf("nullable items projection = %#v", names)
	}
	if any := params.Properties["any"]; any.BooleanSchema == nil || !*any.BooleanSchema {
		t.Fatalf("boolean schema projection = %#v", any)
	}
	if params.Properties["map"].AdditionalPropertiesSchema == nil {
		t.Fatalf("map additionalProperties schema lost: %#v", params.Properties["map"])
	}
	if nested := params.Properties["nested"]; nested.Properties["value"].Type != "string" {
		t.Fatalf("nested object projection = %#v", nested)
	}
	if !exactKeywords(params.Properties["small"].Keywords, []tool.SchemaKeyword{{Name: "maximum", Value: []byte("127")}, {Name: "minimum", Value: []byte("-128")}}) || !exactKeywords(params.Properties["fixed"].Keywords, []tool.SchemaKeyword{{Name: "maxItems", Value: []byte("2")}, {Name: "minItems", Value: []byte("2")}}) {
		t.Fatalf("constraint carriers lost: small=%#v fixed=%#v", params.Properties["small"].Keywords, params.Properties["fixed"].Keywords)
	}
	topmap, ok := registry.Get("remote__topmap")
	if !ok || topmap.Info().Parameters.AdditionalPropertiesSchema == nil {
		t.Fatalf("top-level map AP schema = %#v", topmap)
	}

	ref, ok := registry.Get("remote__ref")
	if !ok {
		t.Fatal("ref tool was not registered")
	}
	refParams := ref.Info().Parameters
	x := refParams.Properties["x"]
	if x.Ref != "#/$defs/x" || len(x.Keywords) != 2 || x.Keywords[0].Name != "a-custom" || string(x.Keywords[0].Value) != "true" || x.Keywords[1].Name != "z-custom" || string(x.Keywords[1].Value) != "1" || len(refParams.Keywords) != 1 || refParams.Keywords[0].Name != "$defs" || len(refParams.AllOf) != 1 {
		t.Fatalf("ref/carrier projection lost: %#v %#v", x, refParams)
	}
}

func TestSchemaRejectionIsTypedAndAtomic(t *testing.T) {
	registry := tool.NewRegistry()
	_, err := prepareTools([]*sdk.Tool{{Name: "same", InputSchema: []any{}}, {Name: "same", InputSchema: map[string]any{"type": "object"}}}, "remote", true, defaultResultDataLimit, registry)
	if err == nil || len(registry.List()) != 0 {
		t.Fatalf("error=%v registered=%d", err, len(registry.List()))
	}
	if got := err.Error(); !(strings.Contains(got, "duplicate final name") && strings.Contains(got, "decoded input schema is not an object")) {
		t.Fatalf("aggregate = %v", err)
	}
}

func TestAllInvalidInventoryIsSorted(t *testing.T) {
	_, err := prepareTools([]*sdk.Tool{
		{Name: "z", InputSchema: []any{}},
		{Name: "a.dot", InputSchema: []any{}},
		{Name: "z", InputSchema: map[string]any{"type": "object"}},
	}, "remote", true, defaultResultDataLimit, tool.NewRegistry())
	if err == nil {
		t.Fatal("all-invalid inventory succeeded")
	}
	got := err.Error()
	for _, want := range []string{"a.dot", "z", "duplicate final name", "decoded input schema is not an object"} {
		if !strings.Contains(got, want) {
			t.Fatalf("aggregate missing %q: %v", want, err)
		}
	}
	if strings.Index(got, "a.dot") > strings.Index(got, "z") {
		t.Fatalf("aggregate is not sorted: %v", err)
	}
}

func TestSchemaPresenceAndCarrierValues(t *testing.T) {
	remote := &sdk.Tool{Name: "presence", InputSchema: map[string]any{
		"type": "object", "a-custom": map[string]any{"v": 1}, "z-custom": true,
		"properties": map[string]any{"absent": map[string]any{}, "ref": map[string]any{"$ref": ""}},
	}}
	wrappers, err := prepareTools([]*sdk.Tool{remote}, "remote", true, defaultResultDataLimit, tool.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	parameters := wrappers[0].Info().Parameters
	if len(parameters.Keywords) != 2 || parameters.Keywords[0].Name != "a-custom" || string(parameters.Keywords[0].Value) != `{"v":1}` || parameters.Keywords[1].Name != "z-custom" || string(parameters.Keywords[1].Value) != "true" {
		t.Fatalf("carrier = %#v", parameters.Keywords)
	}
	encoded, err := parameters.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"absent":{}`) || !strings.Contains(string(encoded), `"ref":{"$ref":""}`) {
		t.Fatalf("presence JSON = %s", encoded)
	}
}

func exactKeywords(got, want []tool.SchemaKeyword) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i].Name != want[i].Name || string(got[i].Value) != string(want[i].Value) {
			return false
		}
	}
	return true
}
