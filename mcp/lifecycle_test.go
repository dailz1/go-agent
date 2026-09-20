package mcpbridge

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/dailz1/go-agent/tool"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestApprovalAnnotationsAndCloseAfterJoin(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := sdk.NewServer(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	readOnly := true
	destructive := true
	server.AddTool(&sdk.Tool{Name: "read", InputSchema: map[string]any{"type": "object"}, Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true}}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{}, nil
	})
	server.AddTool(&sdk.Tool{Name: "write", InputSchema: map[string]any{"type": "object"}, Annotations: &sdk.ToolAnnotations{ReadOnlyHint: readOnly, DestructiveHint: &destructive}}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{}, nil
	})
	server.AddTool(&sdk.Tool{Name: "block", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		close(started)
		<-release
		return &sdk.CallToolResult{}, nil
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
	defer func() { _ = bridge.Close() }()
	for name, want := range map[string]bool{"remote__read": false, "remote__write": true, "remote__block": true} {
		got, _ := registry.Get(name)
		if got.Info().RequiresApproval != want {
			t.Fatalf("%s approval = %v", name, got.Info().RequiresApproval)
		}
	}
	block, _ := registry.Get("remote__block")
	done := make(chan error, 1)
	go func() { _, err := block.Execute(context.Background(), json.RawMessage(`{}`)); done <- err }()
	waitSignal(t, started)
	close(release)
	if err := waitErr(t, done); err != nil {
		t.Fatal(err)
	}
	if err := bridge.Close(); err != nil {
		t.Fatal(err)
	}
	if err := bridge.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := block.Execute(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("Execute after Close returned no hard error")
	}
}

func TestExecutePropagatesContextCancellation(t *testing.T) {
	started := make(chan struct{})
	_, registry := connectTestBridge(t, func(s *sdk.Server) {
		addTool(s, "wait", func(ctx context.Context, _ *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		})
	})
	remote, _ := registry.Get("remote__wait")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := remote.Execute(ctx, json.RawMessage(`{}`)); done <- err }()
	waitSignal(t, started)
	cancel()
	if err := waitErr(t, done); err == nil {
		t.Fatal("canceled Execute returned no hard error")
	} else if !errors.Is(err, context.Canceled) {
		t.Fatalf("errors.Is(context.Canceled) = false: %v", err)
	}
}

func waitSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for signal")
	}
}

func waitErr(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for result")
		return nil
	}
}

func TestNeedsInputIsSoftAndIsNotRetried(t *testing.T) {
	var calls int
	_, registry := connectTestBridge(t, func(s *sdk.Server) {
		addTool(s, "input", func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			calls++
			return &sdk.CallToolResult{InputRequests: sdk.InputRequestMap{"confirm": &sdk.ElicitParams{Message: "continue?"}}}, nil
		})
	})
	remote, _ := registry.Get("remote__input")
	result, err := remote.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil || !result.IsError() || result.Content != "v1 不支持 MCP input-required continuation" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if calls != 1 {
		t.Fatalf("handler calls=%d, want 1", calls)
	}
}

func TestStaleToolIsHardError(t *testing.T) {
	var server *sdk.Server
	bridge, registry := connectTestBridge(t, func(s *sdk.Server) {
		server = s
		addTool(s, "gone", func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return &sdk.CallToolResult{}, nil
		})
	})
	_ = bridge
	remote, _ := registry.Get("remote__gone")
	server.RemoveTools("gone")
	if _, err := remote.Execute(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("stale call returned no hard error")
	}
}
