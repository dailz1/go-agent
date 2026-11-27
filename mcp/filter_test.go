package mcpbridge

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/tool"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func connectFilteredBridge(t *testing.T, filter ToolFilter, add func(*sdk.Server)) (*Bridge, *tool.Registry) {
	t.Helper()
	server := sdk.NewServer(&sdk.Implementation{Name: "test", Version: "1"}, &sdk.ServerOptions{PageSize: 1})
	add(server)
	serverTransport, clientTransport := sdk.NewInMemoryTransports()
	if _, err := server.Connect(context.Background(), serverTransport, nil); err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry()
	bridge, err := Connect(context.Background(), registry, clientTransport, Config{Namespace: "remote"}, WithToolFilter(filter))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bridge.Close() })
	return bridge, registry
}

func TestToolFilterSkipsAtConnect(t *testing.T) {
	bridge, registry := connectFilteredBridge(t,
		func(t *sdk.Tool) bool { return !strings.Contains(t.Description, "secret") },
		func(s *sdk.Server) {
			addDescribedTool(s, "echo", "echo")
			addDescribedTool(s, "secret", "secret admin tool")
			addDescribedTool(s, "second", "second")
		})
	if !sameNames(bridge.Names(), "remote__echo", "remote__second") {
		t.Fatalf("owned = %v, want echo and second", bridge.Names())
	}
	if !sameNames(registryNames(registry), "remote__echo", "remote__second") {
		t.Fatalf("registry = %v, want echo and second", registryNames(registry))
	}
}

func TestToolFilterRefreshStaysFiltered(t *testing.T) {
	server := sdk.NewServer(&sdk.Implementation{Name: "test", Version: "1"}, &sdk.ServerOptions{PageSize: 1})
	addDescribedTool(server, "echo", "echo")
	addDescribedTool(server, "secret", "secret admin tool")
	st, ct := sdk.NewInMemoryTransports()
	if _, err := server.Connect(context.Background(), st, nil); err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry()
	bridge, err := Connect(context.Background(), registry, ct, Config{Namespace: "remote"},
		WithToolFilter(func(t *sdk.Tool) bool { return !strings.Contains(t.Description, "secret") }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bridge.Close() })

	addDescribedTool(server, "extra", "extra")
	summary, err := bridge.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if !sameNames(summary.Added, "remote__extra") {
		t.Fatalf("summary = %+v, want only extra added", summary)
	}
	if !sameNames(registryNames(registry), "remote__echo", "remote__extra") {
		t.Fatalf("registry after Refresh = %v, want secret still filtered", registryNames(registry))
	}
}

func TestToolFilterSkipsAllTools(t *testing.T) {
	bridge, registry := connectFilteredBridge(t,
		func(*sdk.Tool) bool { return false },
		func(s *sdk.Server) {
			addDescribedTool(s, "echo", "echo")
		})
	if len(bridge.Names()) != 0 || len(registryNames(registry)) != 0 {
		t.Fatalf("names = %v registry = %v, want empty", bridge.Names(), registryNames(registry))
	}
	if err := bridge.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestToolFilterDoesNotSuppressNameErrors(t *testing.T) {
	// A filtered-out tool with an unrepresentable final name must still fail
	// the connection: the filter runs after the name check (contract §3.1).
	longName := strings.Repeat("a", 60)
	server := sdk.NewServer(&sdk.Implementation{Name: "test", Version: "1"}, &sdk.ServerOptions{PageSize: 1})
	addDescribedTool(server, longName, "reject me")
	st, ct := sdk.NewInMemoryTransports()
	if _, err := server.Connect(context.Background(), st, nil); err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry()
	_, err := Connect(context.Background(), registry, ct, Config{Namespace: "remote"},
		WithToolFilter(func(*sdk.Tool) bool { return false }))
	if err == nil {
		t.Fatal("Connect succeeded despite an unrepresentable filtered name")
	}
	if !strings.Contains(err.Error(), longName) && !strings.Contains(err.Error(), "name") {
		t.Fatalf("error = %v, want a name error", err)
	}
}

func TestToolFilterRefreshRejectsNewRemoteTool(t *testing.T) {
	server := sdk.NewServer(&sdk.Implementation{Name: "test", Version: "1"}, &sdk.ServerOptions{PageSize: 1})
	addDescribedTool(server, "echo", "echo")
	st, ct := sdk.NewInMemoryTransports()
	if _, err := server.Connect(context.Background(), st, nil); err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry()
	bridge, err := Connect(context.Background(), registry, ct, Config{Namespace: "remote"},
		WithToolFilter(func(t *sdk.Tool) bool { return !strings.Contains(t.Description, "secret") }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bridge.Close() })

	addDescribedTool(server, "secret2", "secret admin tool")
	addDescribedTool(server, "extra", "extra")
	summary, err := bridge.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if !sameNames(summary.Added, "remote__extra") {
		t.Fatalf("summary = %+v, want only extra added", summary)
	}
	if !sameNames(registryNames(registry), "remote__echo", "remote__extra") {
		t.Fatalf("registry = %v, want secret2 filtered", registryNames(registry))
	}
}

type captureHandler struct {
	buf bytes.Buffer
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	fmt.Fprintf(&h.buf, "%s %s", r.Level, r.Message)
	r.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(&h.buf, " %s=%v", a.Key, a.Value)
		return true
	})
	h.buf.WriteByte('\n')
	return nil
}
func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

func TestToolFilterSkipsBeforeConflictCheck(t *testing.T) {
	// Without the filter this Connect fails on a name collision with the
	// user-owned final name; the filter must win by skipping first.
	registry := tool.NewRegistry()
	registry.MustRegister(&userTool{name: "remote__secret"})
	server := sdk.NewServer(&sdk.Implementation{Name: "test", Version: "1"}, &sdk.ServerOptions{PageSize: 1})
	addDescribedTool(server, "echo", "echo")
	addDescribedTool(server, "secret", "secret admin tool")
	st, ct := sdk.NewInMemoryTransports()
	if _, err := server.Connect(context.Background(), st, nil); err != nil {
		t.Fatal(err)
	}
	bridge, err := Connect(context.Background(), registry, ct, Config{Namespace: "remote"},
		WithToolFilter(func(t *sdk.Tool) bool { return !strings.Contains(t.Description, "secret") }))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = bridge.Close() })
	if !sameNames(registryNames(registry), "remote__echo", "remote__secret") {
		t.Fatalf("registry = %v, want user-owned secret plus echo", registryNames(registry))
	}
	if got, _ := registry.Get("remote__secret"); got.Info().Description != "user tool" {
		t.Fatalf("user tool was replaced: description %q", got.Info().Description)
	}
}
