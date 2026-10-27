package mcpbridge

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/dailz1/go-agent/tool"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestCloseDrainsInFlightExecute: an Execute already past the gate completes
// normally while Close runs concurrently; Close returns only after it.
func TestCloseDrainsInFlightExecute(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})

	bridge, registry := connectTestBridge(t, func(s *sdk.Server) {
		addTool(s, "echo", func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			close(started)
			<-release
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "pong"}}}, nil
		})
	})
	wrapper, ok := registry.Get("remote__echo")
	if !ok {
		t.Fatal("remote__echo not registered")
	}

	resCh := make(chan *tool.ToolResult, 1)
	errCh := make(chan error, 1)
	go func() {
		res, err := wrapper.Execute(context.Background(), json.RawMessage(`{}`))
		resCh <- res
		errCh <- err
	}()
	<-started

	closeCh := make(chan error, 1)
	go func() { closeCh <- bridge.Close() }()
	// While the admitted call is blocked, a draining Close must not return.
	select {
	case err := <-closeCh:
		t.Fatalf("Close returned before draining the in-flight call: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)

	if err := <-errCh; err != nil {
		t.Fatalf("in-flight Execute returned hard error: %v", err)
	}
	res := <-resCh
	if res == nil || res.IsError() {
		t.Fatalf("in-flight Execute did not complete normally: %+v", res)
	}
	if err := <-closeCh; err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	if _, still := registry.Get("remote__echo"); still {
		t.Fatal("Close did not unregister remote__echo")
	}
}

// TestExecuteAfterCloseSoftError: Execute after Close is a soft ToolResult
// error, never a Go error.
func TestExecuteAfterCloseSoftError(t *testing.T) {
	bridge, registry := connectTestBridge(t, func(s *sdk.Server) {
		addTool(s, "echo", func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "pong"}}}, nil
		})
	})
	wrapper, ok := registry.Get("remote__echo")
	if !ok {
		t.Fatal("remote__echo not registered")
	}
	if err := bridge.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// A stale handle held past Close must be answered by the gate.
	res, err := wrapper.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute after Close returned hard error: %v", err)
	}
	if res == nil || !res.IsError() {
		t.Fatalf("Execute after Close not a soft error: %+v", res)
	}
}

// TestConcurrentExecuteAndClose shakes the drain under -race: every call
// that passes the gate completes normally despite concurrent Close.
func TestConcurrentExecuteAndClose(t *testing.T) {
	bridge, registry := connectTestBridge(t, func(s *sdk.Server) {
		addTool(s, "echo", func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "pong"}}}, nil
		})
	})
	wrapper, ok := registry.Get("remote__echo")
	if !ok {
		t.Fatal("remote__echo not registered")
	}
	type callOutcome struct {
		res *tool.ToolResult
		err error
	}
	outcomes := make(chan callOutcome, 32)
	done := make(chan struct{}, 32)
	for i := 0; i < 32; i++ {
		go func(i int, w tool.Tool) {
			res, err := w.Execute(context.Background(), json.RawMessage(`{}`))
			outcomes <- callOutcome{res, err}
			done <- struct{}{}
		}(i, wrapper)
	}
	closeErr := bridge.Close()
	for i := 0; i < 32; i++ {
		<-done
	}
	close(outcomes)
	for oc := range outcomes {
		if oc.err != nil {
			t.Errorf("call hard error: %v", oc.err)
		}
		if oc.res == nil {
			t.Error("call nil result")
		}
	}
	if closeErr != nil {
		t.Errorf("Close: %v", closeErr)
	}
}
