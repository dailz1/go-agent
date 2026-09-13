package mcpbridge

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/pkg/tool"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestResultContentAndDataEnvelope(t *testing.T) {
	resource := &sdk.ResourceContents{URI: "file://a\n]b", MIMEType: `x"y`, Text: "body", Blob: []byte("blob")}
	result := &sdk.CallToolResult{Content: []sdk.Content{
		&sdk.TextContent{Text: "text"},
		&sdk.ImageContent{MIMEType: "image/png", Data: []byte{1, 2}},
		&sdk.AudioContent{MIMEType: "audio/wav", Data: []byte{3}},
		&sdk.ResourceLink{Name: "n]", URI: "u\n", Title: "t", MIMEType: "x"},
		&sdk.EmbeddedResource{Resource: resource},
	}, StructuredContent: map[string]any{"ok": true}}
	mapped, err := mapResult(result, defaultResultDataLimit)
	if err != nil {
		t.Fatal(err)
	}
	wantContent := "text\n[mcp:image mimeType=\"image/png\" bytes=2]\n[mcp:audio mimeType=\"audio/wav\" bytes=1]\n" +
		"[mcp:resource_link name=\"n\\]\" uri=\"u\\n\" title=\"t\" mimeType=\"x\"]\n" +
		"[mcp:resource uri=\"file://a\\n\\]b\" mimeType=\"x\\\"y\" bytes=4]\nbody\n{\"ok\":true}"
	if mapped.Content != wantContent {
		t.Fatalf("Content = %q\nwant %q", mapped.Content, wantContent)
	}
	wantData := `{"mcp_content":[{"index":1,"type":"image","mimeType":"image/png","data":"AQI="},{"index":2,"type":"audio","mimeType":"audio/wav","data":"Aw=="},{"index":3,"type":"resource_link","mimeType":"x","uri":"u\n","name":"n]","title":"t"},{"index":4,"type":"resource","mimeType":"x\"y","uri":"file://a\n]b","blob":"YmxvYg=="}]}`
	if string(mapped.Data) != wantData {
		t.Fatalf("Data = %s\nwant %s", mapped.Data, wantData)
	}
}

func TestImageAudioEmptyFieldsAndDataCap(t *testing.T) {
	empty, err := mapResult(&sdk.CallToolResult{Content: []sdk.Content{
		&sdk.ImageContent{}, &sdk.AudioContent{},
	}}, defaultResultDataLimit)
	if err != nil {
		t.Fatal(err)
	}
	wantEmpty := `{"mcp_content":[{"index":0,"type":"image","mimeType":"","data":""},{"index":1,"type":"audio","mimeType":"","data":""}]}`
	if string(empty.Data) != wantEmpty {
		t.Fatalf("empty Data = %s", empty.Data)
	}

	wantOne := `{"mcp_content":[{"index":0,"type":"image","mimeType":"x","data":"YQ=="}]}`
	accepted, err := mapResult(&sdk.CallToolResult{Content: []sdk.Content{&sdk.ImageContent{MIMEType: "x", Data: []byte("a")}}}, int64(len(wantOne)))
	if err != nil || string(accepted.Data) != wantOne {
		t.Fatalf("exact cap: result=%+v err=%v", accepted, err)
	}
	omitted, err := mapResult(&sdk.CallToolResult{Content: []sdk.Content{&sdk.ImageContent{MIMEType: "x", Data: []byte("a")}}}, int64(len(wantOne)-1))
	if err != nil || omitted.Data != nil || omitted.Content != "[mcp:image omitted: bridge data limit]" {
		t.Fatalf("over cap: result=%+v err=%v", omitted, err)
	}
}

func TestDataLimitDefaultAndCumulativeBoundary(t *testing.T) {
	entries := []sdk.Content{&sdk.ImageContent{MIMEType: "x", Data: []byte("a")}, &sdk.AudioContent{MIMEType: "y", Data: []byte("b")}}
	want := `{"mcp_content":[{"index":0,"type":"image","mimeType":"x","data":"YQ=="},{"index":1,"type":"audio","mimeType":"y","data":"Yg=="}]}`
	exact, err := mapResult(&sdk.CallToolResult{Content: entries}, int64(len(want)))
	if err != nil || string(exact.Data) != want {
		t.Fatalf("exact=%+v err=%v", exact, err)
	}
	over, err := mapResult(&sdk.CallToolResult{Content: entries}, int64(len(want)-1))
	if err != nil || string(over.Data) != `{"mcp_content":[{"index":0,"type":"image","mimeType":"x","data":"YQ=="}]}` || over.Content != "[mcp:image mimeType=\"x\" bytes=1]\n[mcp:audio omitted: bridge data limit]" {
		t.Fatalf("over=%+v err=%v", over, err)
	}
}

func TestDefaultDataLimitAppliesThroughBridge(t *testing.T) {
	server := sdk.NewServer(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	server.AddTool(&sdk.Tool{Name: "large", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.ImageContent{MIMEType: "x", Data: make([]byte, 800000)}}}, nil
	})
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
	remote, _ := registry.Get("remote__large")
	result, err := remote.Execute(context.Background(), []byte(`{}`))
	if err != nil || result.Data != nil || result.Content != "[mcp:image omitted: bridge data limit]" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestIsErrorMapsToSoftResult(t *testing.T) {
	_, registry := connectTestBridge(t, func(s *sdk.Server) {
		addTool(s, "error", func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "missing"}}, IsError: true}, nil
		})
	})
	remote, _ := registry.Get("remote__error")
	mapped, err := remote.Execute(context.Background(), []byte(`{}`))
	if err != nil || !mapped.IsError() || mapped.Content != "missing" {
		t.Fatalf("result=%+v err=%v", mapped, err)
	}
}

func TestMetadataAndPresentationFieldsAreDropped(t *testing.T) {
	server := sdk.NewServer(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	server.AddTool(&sdk.Tool{Name: "drop", Description: "kept", Title: "title-hidden", Icons: []sdk.Icon{{Source: "https://icons.invalid/tool-hidden"}}, Meta: sdk.Meta{"meta-hidden": true}, OutputSchema: map[string]any{"output-hidden": true}, Annotations: &sdk.ToolAnnotations{Title: "annotation-hidden"}, InputSchema: map[string]any{"type": "object"}}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{Meta: sdk.Meta{"meta-hidden": true}, Content: []sdk.Content{&sdk.ResourceLink{URI: "x", Name: "n", Meta: sdk.Meta{"meta-hidden": true}, Icons: []sdk.Icon{{Source: "https://icons.invalid/resource-hidden"}}, Annotations: &sdk.Annotations{Audience: []sdk.Role{"assistant"}, LastModified: "2026-01-01T00:00:00Z", Priority: 0.7}}}}, nil
	})
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
	remote, _ := registry.Get("remote__drop")
	infoJSON, err := json.Marshal(remote.Info())
	if err != nil {
		t.Fatal(err)
	}
	result, err := remote.Execute(context.Background(), []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, hidden := range []string{"title-hidden", "output-hidden", "annotation-hidden", "meta-hidden", "tool-hidden", "resource-hidden", "assistant", "2026-01-01"} {
		if strings.Contains(string(infoJSON), hidden) || strings.Contains(string(result.Data), hidden) || strings.Contains(result.Content, hidden) {
			t.Fatalf("dropped field %q leaked: info=%s data=%s content=%s", hidden, infoJSON, result.Data, result.Content)
		}
	}
}

func TestMapResultMapperFailureIsClassified(t *testing.T) {
	_, err := mapResult(&sdk.CallToolResult{Content: []sdk.Content{nil}}, defaultResultDataLimit)
	if _, ok := err.(*mapperError); !ok {
		t.Fatalf("error = %T %v", err, err)
	}
	if _, err := mapResult(&sdk.CallToolResult{StructuredContent: make(chan int)}, defaultResultDataLimit); err == nil {
		t.Fatal("structured marshal succeeded")
	}
}
