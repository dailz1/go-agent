package mcpbridge

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/dailz1/go-agent/pkg/tool"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type mutatingTransport struct {
	base   sdk.Transport
	mutate func(map[string]any)
}

func (t mutatingTransport) Connect(ctx context.Context) (sdk.Connection, error) {
	conn, err := t.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return mutatingConnection{Connection: conn, mutate: t.mutate}, nil
}

type mutatingConnection struct {
	sdk.Connection
	mutate func(map[string]any)
}

func (c mutatingConnection) Write(ctx context.Context, message jsonrpc.Message) error {
	wire, err := jsonrpc.EncodeMessage(message)
	if err != nil {
		return err
	}
	var value map[string]any
	if err := json.Unmarshal(wire, &value); err != nil {
		return err
	}
	if result, ok := value["result"].(map[string]any); ok {
		if _, hasContent := result["content"]; hasContent {
			c.mutate(result)
			wire, err = json.Marshal(value)
			if err != nil {
				return err
			}
			message, err = jsonrpc.DecodeMessage(wire)
			if err != nil {
				return err
			}
		}
	}
	return c.Connection.Write(ctx, message)
}

func TestWireContentDecodeErrorsAreHard(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content any
	}{
		{"unknown discriminator", []any{map[string]any{"type": "future"}}},
		{"nil content element", []any{nil}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := sdk.NewServer(&sdk.Implementation{Name: "test", Version: "1"}, nil)
			addTool(server, "wire", func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
				return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "ok"}}}, nil
			})
			serverTransport, clientTransport := sdk.NewInMemoryTransports()
			if _, err := server.Connect(context.Background(), mutatingTransport{base: serverTransport, mutate: func(result map[string]any) { result["content"] = tc.content }}, nil); err != nil {
				t.Fatal(err)
			}
			registry := tool.NewRegistry()
			bridge, err := Connect(context.Background(), registry, clientTransport, Config{Namespace: "remote"})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = bridge.Close() }()
			remote, _ := registry.Get("remote__wire")
			if _, err := remote.Execute(context.Background(), []byte(`{}`)); err == nil {
				t.Fatal("wire decode error became a tool result")
			}
		})
	}
}
