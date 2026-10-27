package mcpbridge

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/dailz1/go-agent/tool"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func addDescribedTool(s *sdk.Server, name, description string) {
	s.AddTool(&sdk.Tool{Name: name, Description: description, InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: name}}}, nil
	})
}

func sameNames(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func registryNames(registry *tool.Registry) []string {
	infos := registry.List()
	names := make([]string, 0, len(infos))
	for _, info := range infos {
		names = append(names, info.Name)
	}
	return names
}

type userTool struct{ name string }

func (u *userTool) Info() tool.ToolInfo {
	return tool.ToolInfo{Name: u.name, Description: "user tool", Parameters: tool.NewParameterSchema()}
}

func (u *userTool) Execute(context.Context, json.RawMessage) (*tool.ToolResult, error) {
	return tool.NewTextResult("user"), nil
}

func TestRefreshThreeWayReconciliation(t *testing.T) {
	server := sdk.NewServer(&sdk.Implementation{Name: "test", Version: "1"}, &sdk.ServerOptions{PageSize: 1})
	addDescribedTool(server, "keep", "keep")
	addDescribedTool(server, "change", "v1")
	addDescribedTool(server, "drop", "drop")
	st, ct := sdk.NewInMemoryTransports()
	if _, err := server.Connect(context.Background(), st, nil); err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry()
	bridge, err := Connect(context.Background(), registry, ct, Config{Namespace: "remote"}, WithApprovalRequired(false))
	if err != nil {
		t.Fatal(err)
	}
	keptBefore, _ := registry.Get("remote__keep")

	server.RemoveTools("change", "drop")
	addDescribedTool(server, "change", "v2")
	addDescribedTool(server, "extra", "extra")

	summary, err := bridge.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if !sameNames(summary.Added, "remote__extra") || !sameNames(summary.Removed, "remote__drop") ||
		!sameNames(summary.Replaced, "remote__change") || len(summary.Conflicts) != 0 {
		t.Fatalf("summary = %+v", summary)
	}
	if !sameNames(registryNames(registry), "remote__change", "remote__extra", "remote__keep") {
		t.Fatalf("registry after Refresh = %v", registryNames(registry))
	}
	keptAfter, _ := registry.Get("remote__keep")
	if keptBefore != keptAfter {
		t.Fatal("unchanged tool was re-registered instead of kept")
	}
	changed, _ := registry.Get("remote__change")
	if changed.Info().Description != "v2" {
		t.Fatalf("changed description = %q, want v2", changed.Info().Description)
	}
	result, err := changed.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil || result.Content != "change" {
		t.Fatalf("replaced tool execute = %q, %v", result.Content, err)
	}
	if !sameNames(bridge.Names(), "remote__change", "remote__extra", "remote__keep") {
		t.Fatalf("owned set after Refresh = %v", bridge.Names())
	}
	if err := bridge.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if names := registryNames(registry); len(names) != 0 {
		t.Fatalf("registry after Close = %v, want empty", names)
	}
}

func TestRefreshFetchFailureKeepsRegistryAndOwnedSet(t *testing.T) {
	server := sdk.NewServer(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	addDescribedTool(server, "echo", "echo")
	var lists int
	server.AddReceivingMiddleware(func(next sdk.MethodHandler) sdk.MethodHandler {
		return func(ctx context.Context, method string, request sdk.Request) (sdk.Result, error) {
			if method == "tools/list" {
				lists++
				if lists == 2 {
					return nil, context.Canceled
				}
			}
			return next(ctx, method, request)
		}
	})
	st, ct := sdk.NewInMemoryTransports()
	if _, err := server.Connect(context.Background(), st, nil); err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry()
	bridge, err := Connect(context.Background(), registry, ct, Config{Namespace: "remote"}, WithApprovalRequired(false))
	if err != nil {
		t.Fatal(err)
	}
	server.RemoveTools("echo")
	addDescribedTool(server, "fresh", "fresh")

	if _, err := bridge.Refresh(context.Background()); err == nil {
		t.Fatal("Refresh succeeded despite fetch failure")
	}
	if !sameNames(registryNames(registry), "remote__echo") {
		t.Fatalf("registry after failed Refresh = %v", registryNames(registry))
	}
	if !sameNames(bridge.Names(), "remote__echo") {
		t.Fatalf("owned set after failed Refresh = %v", bridge.Names())
	}

	summary, err := bridge.Refresh(context.Background())
	if err != nil {
		t.Fatalf("second Refresh: %v", err)
	}
	if !sameNames(summary.Added, "remote__fresh") || !sameNames(summary.Removed, "remote__echo") {
		t.Fatalf("second summary = %+v", summary)
	}
	if err := bridge.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshSkipsConflictingNamesAndNeverTouchesNonBridgeTools(t *testing.T) {
	server := sdk.NewServer(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	addDescribedTool(server, "a", "a")
	addDescribedTool(server, "b", "b")
	st, ct := sdk.NewInMemoryTransports()
	if _, err := server.Connect(context.Background(), st, nil); err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry()
	if err := registry.Register(&userTool{name: "plain"}); err != nil {
		t.Fatal(err)
	}
	bridge, err := Connect(context.Background(), registry, ct, Config{Namespace: "remote"}, WithApprovalRequired(false))
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Replace(&userTool{name: "remote__a"}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(&userTool{name: "remote__z"}); err != nil {
		t.Fatal(err)
	}
	server.RemoveTools("b")
	addDescribedTool(server, "z", "z")

	summary, err := bridge.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if !sameNames(summary.Conflicts, "remote__a", "remote__z") {
		t.Fatalf("conflicts = %v, want [remote__a remote__z]", summary.Conflicts)
	}
	if len(summary.Added) != 0 || !sameNames(summary.Removed, "remote__b") || len(summary.Replaced) != 0 {
		t.Fatalf("summary = %+v", summary)
	}
	if !sameNames(registryNames(registry), "plain", "remote__a", "remote__z") {
		t.Fatalf("registry after Refresh = %v", registryNames(registry))
	}
	if a, _ := registry.Get("remote__a"); a.Info().Description != "user tool" {
		t.Fatal("conflicting user-replaced name was touched")
	}
	if len(bridge.Names()) != 0 {
		t.Fatalf("owned set after conflicting Refresh = %v, want empty", bridge.Names())
	}

	if err := bridge.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !sameNames(registryNames(registry), "plain", "remote__a", "remote__z") {
		t.Fatalf("registry after Close = %v, want user tools untouched", registryNames(registry))
	}
	if _, ok := registry.Get("remote__a"); !ok {
		t.Fatal("user-replaced name unregistered by Close")
	}
}

func TestCloseUnregistersOwnedAndLeavesUserReplacedNames(t *testing.T) {
	bridge, registry := connectTestBridge(t, func(s *sdk.Server) {
		addTool(s, "echo", func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return &sdk.CallToolResult{}, nil
		})
		addTool(s, "second", func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return &sdk.CallToolResult{}, nil
		})
	})
	if err := registry.Register(&userTool{name: "plain"}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Replace(&userTool{name: "remote__second"}); err != nil {
		t.Fatal(err)
	}
	if err := bridge.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := bridge.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if !sameNames(registryNames(registry), "plain", "remote__second") {
		t.Fatalf("registry after Close = %v", registryNames(registry))
	}
	if _, ok := registry.Get("remote__echo"); ok {
		t.Fatal("owned name survived Close")
	}
}

func TestUnregisterOwnedChecksIdentity(t *testing.T) {
	registry := tool.NewRegistry()
	ours := &remoteTool{info: tool.ToolInfo{Name: "remote__ours", Parameters: tool.NewParameterSchema()}}
	replaced := &remoteTool{info: tool.ToolInfo{Name: "remote__replaced", Parameters: tool.NewParameterSchema()}}
	freed := &remoteTool{info: tool.ToolInfo{Name: "remote__freed", Parameters: tool.NewParameterSchema()}}
	registry.MustRegister(ours)
	registry.MustRegister(replaced)
	registry.MustRegister(freed)
	if err := registry.Replace(&userTool{name: "remote__replaced"}); err != nil {
		t.Fatal(err)
	}
	registry.Unregister("remote__freed")

	unregisterOwned(registry, map[string]*remoteTool{
		"remote__ours":     ours,
		"remote__replaced": replaced,
		"remote__freed":    freed,
	})

	if _, ok := registry.Get("remote__ours"); ok {
		t.Fatal("owned name survived unregisterOwned")
	}
	if _, ok := registry.Get("remote__replaced"); !ok {
		t.Fatal("user-replaced name was unregistered")
	}
	if _, ok := registry.Get("remote__freed"); ok {
		t.Fatal("freed name came back")
	}
}
