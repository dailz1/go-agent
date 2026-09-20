package mcpbridge

import (
	"context"
	"encoding/json"
	"iter"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dailz1/go-agent/agent"
	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func connectTestBridge(t *testing.T, add func(*sdk.Server)) (*Bridge, *tool.Registry) {
	t.Helper()
	server := sdk.NewServer(&sdk.Implementation{Name: "test", Version: "1"}, &sdk.ServerOptions{PageSize: 1})
	add(server)
	serverTransport, clientTransport := sdk.NewInMemoryTransports()
	if _, err := server.Connect(context.Background(), serverTransport, nil); err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry()
	bridge, err := Connect(context.Background(), registry, clientTransport, Config{Namespace: "remote"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bridge.Close() })
	return bridge, registry
}

func addTool(s *sdk.Server, name string, h sdk.ToolHandler) {
	s.AddTool(&sdk.Tool{Name: name, Description: name, InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}}, h)
}

func TestConnectDiscoversPagesAndRunsThroughAgent(t *testing.T) {
	var called atomic.Int32
	bridge, registry := connectTestBridge(t, func(s *sdk.Server) {
		addTool(s, "echo", func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			called.Add(1)
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "pong"}}}, nil
		})
		addTool(s, "second", func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return &sdk.CallToolResult{}, nil
		})
	})
	if got := bridge.Names(); len(got) != 2 || got[0] != "remote__echo" || got[1] != "remote__second" {
		t.Fatalf("Names() = %v", got)
	}
	provider := &roundTripProvider{}
	out, err := agent.New(provider, registry, agent.WithApprovalFn(func(tool.ToolInfo, json.RawMessage) bool { return true })).Run(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if out.ToolCalls != 1 || called.Load() != 1 {
		t.Fatalf("tool calls=%d handler calls=%d", out.ToolCalls, called.Load())
	}
}

type observeTransport struct{ connected atomic.Bool }

func (t *observeTransport) Connect(context.Context) (sdk.Connection, error) {
	t.connected.Store(true)
	return nil, context.Canceled
}

func TestInvalidResultDataLimitDoesNotConnect(t *testing.T) {
	transport := new(observeTransport)
	_, err := Connect(context.Background(), tool.NewRegistry(), transport, Config{Namespace: "remote"}, WithResultDataLimit(0))
	if err == nil || transport.connected.Load() {
		t.Fatalf("err=%v connected=%v", err, transport.connected.Load())
	}
	_, err = Connect(context.Background(), tool.NewRegistry(), transport, Config{Namespace: "remote"}, WithResultDataLimit(-1))
	if err == nil || transport.connected.Load() {
		t.Fatalf("err=%v connected=%v", err, transport.connected.Load())
	}
}

func TestDefaultApprovalFailsClosedBeforeRPC(t *testing.T) {
	var called atomic.Int32
	_, registry := connectTestBridge(t, func(s *sdk.Server) {
		addTool(s, "echo", func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			called.Add(1)
			return &sdk.CallToolResult{}, nil
		})
	})
	if _, err := agent.New(&roundTripProvider{}, registry).Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if got := called.Load(); got != 0 {
		t.Fatalf("handler calls=%d, want 0", got)
	}
}

func TestInvalidArgumentsAreSoftWithoutRPC(t *testing.T) {
	var called atomic.Int32
	_, registry := connectTestBridge(t, func(s *sdk.Server) {
		addTool(s, "echo", func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			called.Add(1)
			return &sdk.CallToolResult{}, nil
		})
	})
	remote, _ := registry.Get("remote__echo")
	for _, args := range []json.RawMessage{nil, []byte("null"), []byte("[]"), []byte("1"), []byte("{")} {
		result, err := remote.Execute(context.Background(), args)
		if err != nil || !result.IsError() {
			t.Fatalf("args %q: result=%+v err=%v", args, result, err)
		}
	}
	if got := called.Load(); got != 0 {
		t.Fatalf("handler calls = %d, want 0", got)
	}
}

func TestAgentParallelCallsShareBridgeSession(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	var calls atomic.Int32
	_, registry := connectTestBridge(t, func(s *sdk.Server) {
		addTool(s, "echo", func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			calls.Add(1)
			started <- struct{}{}
			<-release
			return &sdk.CallToolResult{}, nil
		})
	})
	done := make(chan error, 1)
	go func() {
		_, err := agent.New(&parallelProvider{}, registry, agent.WithToolConcurrency(2), agent.WithApprovalFn(func(tool.ToolInfo, json.RawMessage) bool { return true })).Run(context.Background(), "go")
		done <- err
	}()
	waitSignal(t, started)
	waitSignal(t, started)
	close(release)
	if err := waitErr(t, done); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("handler calls=%d", got)
	}
}

func TestBridgeSessionSupportsConcurrentCalls(t *testing.T) {
	var calls atomic.Int32
	_, registry := connectTestBridge(t, func(s *sdk.Server) {
		addTool(s, "echo", func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			calls.Add(1)
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "ok"}}}, nil
		})
	})
	remote, _ := registry.Get("remote__echo")
	done := make(chan error, 2)
	for range 2 {
		go func() { _, err := remote.Execute(context.Background(), json.RawMessage(`{}`)); done <- err }()
	}
	for range 2 {
		if err := waitErr(t, done); err != nil {
			t.Fatal(err)
		}
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("handler calls=%d", got)
	}
}

func TestRegistryIsPublishedOnlyAfterConnectReturns(t *testing.T) {
	server := sdk.NewServer(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	addTool(server, "one", func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{}, nil
	})
	addTool(server, "two", func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{}, nil
	})
	listed := make(chan struct{})
	release := make(chan struct{})
	server.AddReceivingMiddleware(func(next sdk.MethodHandler) sdk.MethodHandler {
		return func(ctx context.Context, method string, request sdk.Request) (sdk.Result, error) {
			if method == "tools/list" {
				close(listed)
				<-release
			}
			return next(ctx, method, request)
		}
	})
	st, ct := sdk.NewInMemoryTransports()
	if _, err := server.Connect(context.Background(), st, nil); err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry()
	type connectResult struct {
		bridge *Bridge
		err    error
	}
	connected := make(chan connectResult, 1)
	go func() {
		bridge, err := Connect(context.Background(), registry, ct, Config{Namespace: "remote"})
		connected <- connectResult{bridge, err}
	}()
	waitSignal(t, listed)
	close(release)
	var result connectResult
	select {
	case result = <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for Connect")
	}
	if result.err != nil {
		t.Fatal(result.err)
	}
	defer func() { _ = result.bridge.Close() }()
	readerStart := make(chan struct{})
	readerDone := make(chan []tool.ToolInfo, 1)
	go func() { <-readerStart; readerDone <- registry.List() }()
	close(readerStart)
	var got []tool.ToolInfo
	select {
	case got = <-readerDone:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for reader")
	}
	if len(got) != 2 || got[0].Name != "remote__one" || got[1].Name != "remote__two" {
		t.Fatalf("published tools=%v", got)
	}
}

func TestMiddlePageFailureIsAtomic(t *testing.T) {
	server := sdk.NewServer(&sdk.Implementation{Name: "test", Version: "1"}, &sdk.ServerOptions{PageSize: 1})
	addTool(server, "one", func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{}, nil
	})
	addTool(server, "two", func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{}, nil
	})
	var lists atomic.Int32
	server.AddReceivingMiddleware(func(next sdk.MethodHandler) sdk.MethodHandler {
		return func(ctx context.Context, method string, request sdk.Request) (sdk.Result, error) {
			if method == "tools/list" && lists.Add(1) == 2 {
				return nil, context.Canceled
			}
			return next(ctx, method, request)
		}
	})
	st, ct := sdk.NewInMemoryTransports()
	if _, err := server.Connect(context.Background(), st, nil); err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry()
	if _, err := Connect(context.Background(), registry, ct, Config{Namespace: "remote"}); err == nil {
		t.Fatal("Connect succeeded after middle-page error")
	}
	if got := len(registry.List()); got != 0 {
		t.Fatalf("registered %d tools", got)
	}
}

func TestTransportDeathIsHard(t *testing.T) {
	server := sdk.NewServer(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	addTool(server, "echo", func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{}, nil
	})
	st, ct := sdk.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry()
	bridge, err := Connect(context.Background(), registry, ct, Config{Namespace: "remote"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = bridge.Close() }()
	remote, _ := registry.Get("remote__echo")
	if err := serverSession.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := remote.Execute(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("dead transport was not hard")
	}
}

func TestNameBoundariesAndAtomicFailure(t *testing.T) {
	for _, tc := range []struct {
		remote, namespace string
		ok                bool
	}{
		{"x", "remote", true}, {"has.dot", "remote", false}, {"é", "remote", false},
		{strings.Repeat("a", 56), "remote", true}, {strings.Repeat("a", 57), "remote", false},
		{strings.Repeat("a", 128), "remote", false}, {"tool", strings.Repeat("n", 60), false},
	} {
		server := sdk.NewServer(&sdk.Implementation{Name: "test", Version: "1"}, nil)
		addTool(server, tc.remote, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return &sdk.CallToolResult{}, nil
		})
		st, ct := sdk.NewInMemoryTransports()
		if _, err := server.Connect(context.Background(), st, nil); err != nil {
			t.Fatal(err)
		}
		registry := tool.NewRegistry()
		bridge, err := Connect(context.Background(), registry, ct, Config{Namespace: tc.namespace})
		if (err == nil) != tc.ok || (err != nil && len(registry.List()) != 0) {
			t.Fatalf("remote=%q namespace=%q err=%v registry=%v", tc.remote, tc.namespace, err, registry.List())
		}
		if bridge != nil {
			_ = bridge.Close()
		}
	}
}

type parallelProvider struct{ n int }

func (*parallelProvider) Name() string { return "parallel" }
func (p *parallelProvider) Chat(_ context.Context, _ []llm.Message, _ []tool.ToolInfo, _ ...llm.Option) (*llm.Message, *llm.Usage, error) {
	p.n++
	if p.n == 1 {
		message := llm.AssistantToolCallMessage(llm.ToolUseBlock{ID: "1", Name: "remote__echo", Input: json.RawMessage(`{}`)}, llm.ToolUseBlock{ID: "2", Name: "remote__echo", Input: json.RawMessage(`{}`)})
		return &message, nil, nil
	}
	message := llm.AssistantMessage("done")
	return &message, nil, nil
}
func (*parallelProvider) ChatStream(context.Context, []llm.Message, []tool.ToolInfo, ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	return nil, llm.ErrStreamingNotSupported
}

type roundTripProvider struct{ n int }

func (*roundTripProvider) Name() string { return "round-trip" }
func (p *roundTripProvider) Chat(_ context.Context, _ []llm.Message, _ []tool.ToolInfo, _ ...llm.Option) (*llm.Message, *llm.Usage, error) {
	p.n++
	if p.n == 1 {
		message := llm.AssistantToolCallMessage(llm.ToolUseBlock{ID: "1", Name: "remote__echo", Input: json.RawMessage(`{}`)})
		return &message, nil, nil
	}
	message := llm.AssistantMessage("done")
	return &message, nil, nil
}
func (*roundTripProvider) ChatStream(context.Context, []llm.Message, []tool.ToolInfo, ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	return nil, llm.ErrStreamingNotSupported
}
