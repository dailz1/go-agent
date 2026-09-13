package mcpbridge

import (
	"context"
	"testing"

	"github.com/dailz1/go-agent/pkg/tool"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type pointerRoot struct {
	Value string `json:"value"`
}
type recursiveRoot struct {
	Next *recursiveRoot `json:"next"`
}
type numericRoots struct {
	I16  int16   `json:"i16"`
	I32  int32   `json:"i32"`
	Uint uint    `json:"uint"`
	U8   uint8   `json:"u8"`
	U16  uint16  `json:"u16"`
	U32  uint32  `json:"u32"`
	U64  uint64  `json:"u64"`
	UP   uintptr `json:"up"`
}

func TestSDKTopLevelAndBoundSchemaMatrix(t *testing.T) {
	bridge, registry := connectTestBridge(t, func(s *sdk.Server) {
		sdk.AddTool(s, &sdk.Tool{Name: "pointer"}, func(context.Context, *sdk.CallToolRequest, *pointerRoot) (*sdk.CallToolResult, any, error) {
			return nil, nil, nil
		})
		sdk.AddTool(s, &sdk.Tool{Name: "any"}, func(context.Context, *sdk.CallToolRequest, any) (*sdk.CallToolResult, any, error) {
			return nil, nil, nil
		})
		sdk.AddTool(s, &sdk.Tool{Name: "bounds"}, func(context.Context, *sdk.CallToolRequest, numericRoots) (*sdk.CallToolResult, any, error) {
			return nil, nil, nil
		})
		enumSchema := map[string]any{"type": "object", "properties": map[string]any{
			"value": map[string]any{"type": "string", "enum": []any{"a", "b"}},
		}}
		sdk.AddTool(s, &sdk.Tool{Name: "enum", InputSchema: enumSchema}, func(context.Context, *sdk.CallToolRequest, any) (*sdk.CallToolResult, any, error) {
			return nil, nil, nil
		})
		customSchema := map[string]any{"type": "object", "properties": map[string]any{
			"name": map[string]any{"type": "string", "pattern": "^[a-z]+$", "format": "hostname", "default": "host"},
		}}
		s.AddTool(&sdk.Tool{Name: "custom", InputSchema: customSchema}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return &sdk.CallToolResult{}, nil
		})
	})
	defer func() { _ = bridge.Close() }()
	pointer, _ := registry.Get("remote__pointer")
	if root := pointer.Info().Parameters; root.Type != "object" || root.Properties["value"].Type != "string" {
		t.Fatalf("pointer root=%#v", root)
	}
	anyTool, _ := registry.Get("remote__any")
	if root := anyTool.Info().Parameters; root.Type != "object" || len(root.Properties) != 0 {
		t.Fatalf("any root=%#v", root)
	}
	bounds, _ := registry.Get("remote__bounds")
	for name, want := range map[string][]tool.SchemaKeyword{
		"i16":  {{Name: "maximum", Value: []byte("32767")}, {Name: "minimum", Value: []byte("-32768")}},
		"i32":  {{Name: "maximum", Value: []byte("2147483647")}, {Name: "minimum", Value: []byte("-2147483648")}},
		"uint": {{Name: "minimum", Value: []byte("0")}},
		"u8":   {{Name: "maximum", Value: []byte("255")}, {Name: "minimum", Value: []byte("0")}},
		"u16":  {{Name: "maximum", Value: []byte("65535")}, {Name: "minimum", Value: []byte("0")}},
		"u32":  {{Name: "maximum", Value: []byte("4294967295")}, {Name: "minimum", Value: []byte("0")}},
		"u64":  {{Name: "minimum", Value: []byte("0")}},
		"up":   {{Name: "minimum", Value: []byte("0")}},
	} {
		if got := bounds.Info().Parameters.Properties[name].Keywords; !exactKeywords(got, want) {
			t.Fatalf("%s bounds=%#v", name, got)
		}
	}
	enumTool, _ := registry.Get("remote__enum")
	if got := enumTool.Info().Parameters.Properties["value"].Enum; len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("enum=%#v", got)
	}
	custom, _ := registry.Get("remote__custom")
	if got := custom.Info().Parameters.Properties["name"].Keywords; !exactKeywords(got, []tool.SchemaKeyword{{Name: "default", Value: []byte(`"host"`)}, {Name: "format", Value: []byte(`"hostname"`)}, {Name: "pattern", Value: []byte(`"^[a-z]+$"`)}}) {
		t.Fatalf("custom carriers=%#v", got)
	}
}

func TestSDKRecursiveInputFailsBeforeDiscovery(t *testing.T) {
	server := sdk.NewServer(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	panicked := false
	func() {
		defer func() { panicked = recover() != nil }()
		sdk.AddTool(server, &sdk.Tool{Name: "recursive"}, func(context.Context, *sdk.CallToolRequest, recursiveRoot) (*sdk.CallToolResult, any, error) {
			return nil, nil, nil
		})
	}()
	if !panicked {
		t.Fatal("recursive AddTool did not panic")
	}
	st, ct := sdk.NewInMemoryTransports()
	if _, err := server.Connect(context.Background(), st, nil); err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry()
	bridge, err := Connect(context.Background(), registry, ct, Config{Namespace: "remote"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = bridge.Close() }()
	if names := bridge.Names(); len(names) != 0 {
		t.Fatalf("recursive tool published: %v", names)
	}
}

func TestOfficialVocabularyInventoryPasses(t *testing.T) {
	bridge, registry := connectTestBridge(t, func(s *sdk.Server) {
		for name, property := range map[string]map[string]any{
			"read_file": {"type": "string"}, "read_graph": {"type": "string"},
			"read_multiple_files": {"type": "array", "items": map[string]any{"type": "string"}, "minItems": 1},
			"search_nodes":        {"type": "string", "maxLength": 1000},
			"edit_file":           {"type": "boolean", "default": false},
			"sequential_thinking": {"type": "number", "minimum": 1},
		} {
			s.AddTool(&sdk.Tool{Name: name, InputSchema: map[string]any{"type": "object", "properties": map[string]any{"value": property}}}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
				return &sdk.CallToolResult{}, nil
			})
		}
	})
	defer func() { _ = bridge.Close() }()
	for name, want := range map[string][]tool.SchemaKeyword{
		"read_file": nil, "read_graph": nil,
		"read_multiple_files": {{Name: "minItems", Value: []byte("1")}},
		"search_nodes":        {{Name: "maxLength", Value: []byte("1000")}},
		"edit_file":           {{Name: "default", Value: []byte("false")}},
		"sequential_thinking": {{Name: "minimum", Value: []byte("1")}},
	} {
		item, ok := registry.Get("remote__" + name)
		if !ok || !exactKeywords(item.Info().Parameters.Properties["value"].Keywords, want) {
			t.Fatalf("%s schema=%#v", name, item)
		}
	}
}
