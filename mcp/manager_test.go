package mcpbridge

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/dailz1/go-agent/tool"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type localStubTool struct {
	Name string
}

func (t localStubTool) Info() tool.ToolInfo {
	return tool.ToolInfo{Name: t.Name, Description: "local takeover", Parameters: tool.NewParameterSchema()}
}

func (t localStubTool) Execute(context.Context, json.RawMessage) (*tool.ToolResult, error) {
	return &tool.ToolResult{}, nil
}

func connectManagerServer(t *testing.T, m *Manager, id, toolName string) (*sdk.Server, error) {
	t.Helper()
	server := sdk.NewServer(&sdk.Implementation{Name: id, Version: "1"}, nil)
	addTool(server, toolName, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "pong"}}}, nil
	})
	st, ct := sdk.NewInMemoryTransports()
	if _, err := server.Connect(context.Background(), st, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Disconnect(id) })
	err := m.Connect(context.Background(), id, ct, Config{Namespace: id})
	return server, err
}

func TestManagerAttributionAndSnapshot(t *testing.T) {
	registry := tool.NewRegistry()
	m := NewManager(registry)
	if _, err := connectManagerServer(t, m, "alpha", "echo"); err != nil {
		t.Fatal(err)
	}
	if _, err := connectManagerServer(t, m, "beta", "echo"); err != nil {
		t.Fatal(err)
	}

	if _, ok := registry.Get("alpha__echo"); !ok {
		t.Fatal("alpha__echo not registered")
	}
	if id, ok := m.OwnerOf("alpha__echo"); !ok || id != "alpha" {
		t.Fatalf("OwnerOf(alpha__echo) = %q, %v", id, ok)
	}
	if id, ok := m.OwnerOf("beta__echo"); !ok || id != "beta" {
		t.Fatalf("OwnerOf(beta__echo) = %q, %v", id, ok)
	}
	if _, ok := m.OwnerOf("nope__x"); ok {
		t.Fatal("OwnerOf attributed an unknown tool")
	}

	views := m.Snapshot()
	if len(views) != 2 || views[0].ServerID != "alpha" || views[1].ServerID != "beta" {
		t.Fatalf("Snapshot = %+v", views)
	}
	if len(views[0].ToolNames) != 1 || views[0].ToolNames[0] != "alpha__echo" {
		t.Fatalf("alpha view = %+v", views[0])
	}

	if err := m.Disconnect("alpha"); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if _, still := registry.Get("alpha__echo"); still {
		t.Fatal("Disconnect did not unregister alpha__echo")
	}
	if _, ok := m.OwnerOf("alpha__echo"); ok {
		t.Fatal("OwnerOf attributed a disconnected server's tool")
	}
	if _, ok := m.OwnerOf("beta__echo"); !ok {
		t.Fatal("OwnerOf lost beta__echo after disconnecting alpha")
	}
}

func TestManagerLifecycleStateTransitions(t *testing.T) {
	registry := tool.NewRegistry()
	m := NewManager(registry)
	if _, err := connectManagerServer(t, m, "alpha", "echo"); err != nil {
		t.Fatal(err)
	}

	if err := m.Connect(context.Background(), "alpha", nil, Config{Namespace: "alpha"}); err == nil {
		t.Fatal("duplicate serverID accepted")
	}
	if err := m.Disconnect("gamma"); !errors.Is(err, ErrUnknownServer) {
		t.Fatalf("Disconnect(unknown) = %v", err)
	}
	if _, err := m.Refresh(context.Background(), "gamma"); !errors.Is(err, ErrUnknownServer) {
		t.Fatalf("Refresh(unknown) = %v", err)
	}
	if err := m.Disconnect("alpha"); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	// A failed Connect reserves no ID: a valid retry on the same ID succeeds.
	if err := m.Connect(context.Background(), "alpha", nil, Config{Namespace: "alpha"}); err == nil {
		t.Fatal("Connect with nil transport accepted")
	}
	if _, err := connectManagerServer(t, m, "alpha", "echo"); err != nil {
		t.Fatalf("retry Connect after failed attempt: %v", err)
	}
	if _, ok := registry.Get("alpha__echo"); !ok {
		t.Fatal("alpha__echo not registered after retry")
	}
	if err := m.Disconnect("alpha"); err != nil {
		t.Fatalf("Disconnect after retry: %v", err)
	}
	if err := m.Disconnect("alpha"); err == nil {
		t.Fatal("failed Connect left a manageable entry behind")
	}
	if views := m.Snapshot(); len(views) != 0 {
		t.Fatalf("Snapshot after failed reconnect = %+v", views)
	}
}

// TestManagerRefreshReconcilesAttribution: a successful Refresh removes the
// vanished tool from attribution and adds the new one.
func TestManagerRefreshReconcilesAttribution(t *testing.T) {
	registry := tool.NewRegistry()
	m := NewManager(registry)
	server, err := connectManagerServer(t, m, "alpha", "old")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.OwnerOf("alpha__old"); !ok {
		t.Fatal("alpha__old not attributed before Refresh")
	}

	server.RemoveTools("old")
	addTool(server, "new", func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{}, nil
	})
	summary, err := m.Refresh(context.Background(), "alpha")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if len(summary.Removed) != 1 || summary.Removed[0] != "alpha__old" {
		t.Fatalf("summary.Removed = %v", summary.Removed)
	}
	if len(summary.Added) != 1 || summary.Added[0] != "alpha__new" {
		t.Fatalf("summary.Added = %v", summary.Added)
	}
	if _, ok := m.OwnerOf("alpha__old"); ok {
		t.Fatal("OwnerOf still attributes the removed tool after Refresh")
	}
	if id, ok := m.OwnerOf("alpha__new"); !ok || id != "alpha" {
		t.Fatalf("OwnerOf(alpha__new) = %q, %v", id, ok)
	}
	if _, still := registry.Get("alpha__old"); still {
		t.Fatal("Refresh did not unregister alpha__old")
	}
}

func TestManagerOwnerOfExcludesUserReplacedNames(t *testing.T) {
	registry := tool.NewRegistry()
	m := NewManager(registry)
	if _, err := connectManagerServer(t, m, "alpha", "echo"); err != nil {
		t.Fatal(err)
	}

	local := localStubTool{Name: "alpha__echo"}
	if err := registry.Replace(local); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if _, ok := m.OwnerOf("alpha__echo"); ok {
		t.Fatal("OwnerOf attributed a user-replaced name to the bridge")
	}
}

func TestManagerConcurrentLifecycleAndQueries(t *testing.T) {
	registry := tool.NewRegistry()
	m := NewManager(registry)
	if _, err := connectManagerServer(t, m, "alpha", "echo"); err != nil {
		t.Fatal(err)
	}

	// Overlap Refresh's owned-map replacement with live queries.
	release := make(chan struct{})
	var refreshErr error
	go func() {
		for i := 0; i < 20; i++ {
			if _, err := m.Refresh(context.Background(), "alpha"); err != nil {
				refreshErr = err
				break
			}
		}
		release <- struct{}{}
	}()
	for i := 0; i < 50; i++ {
		m.Snapshot()
		m.OwnerOf("alpha__echo")
	}
	<-release
	if refreshErr != nil {
		t.Fatalf("concurrent Refresh: %v", refreshErr)
	}

	for i := 0; i < 10; i++ {
		if _, err := connectManagerServer(t, m, "beta", "echo"); err != nil {
			t.Fatalf("Connect(beta) iteration %d: %v", i, err)
		}
		if err := m.Disconnect("beta"); err != nil {
			t.Fatalf("Disconnect(beta) iteration %d: %v", i, err)
		}
	}
}
