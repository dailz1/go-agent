package mcpbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/dailz1/go-agent/tool"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Bridge owns one MCP client session and the set of registry names it
// registered (the owned set). During Connect and Refresh, no concurrent
// external registry mutation may touch the same namespace.
//
// Close is a graceful drain: it first rejects new Execute calls with a soft
// error, then waits for in-flight Execute calls to finish before
// unregistering and closing the session. Execute blocked on a hung remote
// call delays Close; cancel the call's context to break it.
type Bridge struct {
	session          *sdk.ClientSession
	registry         *tool.Registry
	namespace        string
	approvalRequired bool
	dataLimit        int64
	owned            map[string]*remoteTool
	lc               *lifecycle
}

// lifecycle gates Execute against Close. closing is published atomically
// before draining so new Execute calls reject immediately even while Close
// waits on a hung in-flight call; mu only counts admitted calls — Execute
// holds its read lock for the whole call, Close's write lock returns once
// every admitted call has finished.
type lifecycle struct {
	mu      sync.RWMutex
	closing atomic.Bool
}

// Connect discovers every paginated MCP tool and registers static wrappers.
// It does not auto-subscribe to list-changed; reconcile later changes with
// Refresh. A failed Connect unregisters whatever it already registered.
func Connect(ctx context.Context, registry *tool.Registry, transport sdk.Transport, cfg Config, opts ...Option) (*Bridge, error) {
	if registry == nil {
		return nil, fmt.Errorf("mcp registry is nil")
	}
	if transport == nil {
		return nil, fmt.Errorf("mcp transport is nil")
	}
	if !validNamespace(cfg.Namespace) {
		return nil, fmt.Errorf("mcp namespace %q must match ^[A-Za-z0-9_-]{1,64}$ and not contain __", cfg.Namespace)
	}
	options, err := resolveConfig(opts)
	if err != nil {
		return nil, err
	}

	client := sdk.NewClient(&sdk.Implementation{Name: "go-agent-mcp", Version: "v1"}, &sdk.ClientOptions{
		Capabilities:   &sdk.ClientCapabilities{},
		MultiRoundTrip: &sdk.MultiRoundTripOptions{Disabled: true},
	})
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("connect MCP client: %w", err)
	}
	lc := &lifecycle{}
	closeOnError := true
	owned := make(map[string]*remoteTool)
	defer func() {
		if closeOnError {
			unregisterOwned(registry, owned)
			_ = session.Close()
		}
	}()

	remote, err := listTools(ctx, session)
	if err != nil {
		return nil, fmt.Errorf("list MCP tools: %w", err)
	}
	wrappers, err := prepareTools(remote, cfg.Namespace, options.approvalRequired, options.resultDataLimit, registry)
	if err != nil {
		return nil, err
	}
	for _, wrapper := range wrappers {
		wrapper.session = session
		wrapper.lc = lc
		if err := registry.Register(wrapper); err != nil {
			return nil, fmt.Errorf("register MCP tool %q: %w", wrapper.info.Name, err)
		}
		owned[wrapper.info.Name] = wrapper
	}
	closeOnError = false
	return &Bridge{
		session:          session,
		registry:         registry,
		namespace:        cfg.Namespace,
		approvalRequired: options.approvalRequired,
		dataLimit:        options.resultDataLimit,
		owned:            owned,
		lc:               lc,
	}, nil
}

func listTools(ctx context.Context, session *sdk.ClientSession) ([]*sdk.Tool, error) {
	var tools []*sdk.Tool
	var cursor string
	for {
		page, err := session.ListTools(ctx, &sdk.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, err
		}
		if page == nil {
			return nil, fmt.Errorf("nil tools/list result")
		}
		tools = append(tools, page.Tools...)
		if page.NextCursor == "" {
			return tools, nil
		}
		cursor = page.NextCursor
	}
}

func prepareTools(remote []*sdk.Tool, namespace string, approvalRequired bool, dataLimit int64, registry *tool.Registry) ([]*remoteTool, error) {
	existing := make(map[string]struct{})
	for _, info := range registry.List() {
		existing[info.Name] = struct{}{}
	}
	seen := make(map[string]struct{})
	wrappers := make([]*remoteTool, 0, len(remote))
	var errs []error
	for _, item := range remote {
		remoteName := ""
		if item != nil {
			remoteName = item.Name
		}
		name := namespace + "__" + remoteName
		if _, err := finalName(namespace, remoteName); err != nil {
			errs = append(errs, err)
		}
		if _, duplicate := seen[name]; duplicate {
			errs = append(errs, &NameNotRepresentableError{Remote: remoteName, Final: name, Reason: "duplicate final name"})
		}
		seen[name] = struct{}{}
		if _, collision := existing[name]; collision {
			errs = append(errs, &NameNotRepresentableError{Remote: remoteName, Final: name, Reason: "already registered"})
		}

		schema, err := projectSchema(item)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if finalNamePattern.MatchString(name) {
			wrapper := newRemoteTool(item, name, schema, approvalRequired)
			wrapper.dataLimit = dataLimit
			wrappers = append(wrappers, wrapper)
		}
	}
	if err := sortedErrors(errs); err != nil {
		return nil, err
	}
	return wrappers, nil
}

// Names returns sorted copies of the names in the bridge's owned set whose
// registry entry is still this bridge's wrapper (names a user took over with
// Replace are excluded, matching what Close would remove).
func (b *Bridge) Names() []string {
	if b == nil || len(b.owned) == 0 {
		return nil
	}
	names := make([]string, 0, len(b.owned))
	for name, wrapper := range b.owned {
		if current, ok := b.registry.Get(name); ok && current == tool.Tool(wrapper) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// Close drains and tears the bridge down. It first sets the closing gate so
// new Execute calls fail softly, then waits for in-flight Execute calls to
// return — Execute blocked on a hung remote call delays Close, so cancel the
// call's context to break it. Once drained it unregisters every still-owned
// name whose current registry entry is still this bridge's wrapper (names
// taken over by a user Replace are left in place) and delegates to the SDK
// session, reporting only the session error.
func (b *Bridge) Close() error {
	if b == nil || b.session == nil {
		return nil
	}
	b.lc.closing.Store(true)
	b.lc.mu.Lock()
	b.lc.mu.Unlock()
	unregisterOwned(b.registry, b.owned)
	return b.session.Close()
}

// unregisterOwned removes every owned name whose current registry entry is
// still the owning bridge's wrapper. Names replaced or freed by the user
// are not owned anymore and stay untouched. Comparing the *remoteTool
// pointers through the Tool interface is safe: differing dynamic types
// compare unequal without panicking.
func unregisterOwned(registry *tool.Registry, owned map[string]*remoteTool) {
	for name, wrapper := range owned {
		if current, ok := registry.Get(name); ok && current == tool.Tool(wrapper) {
			registry.Unregister(name)
		}
	}
}

type remoteTool struct {
	session    *sdk.ClientSession
	lc         *lifecycle
	remoteName string
	info       tool.ToolInfo
	dataLimit  int64
}

func (t *remoteTool) Info() tool.ToolInfo { return t.info }

func (t *remoteTool) Execute(ctx context.Context, raw json.RawMessage) (*tool.ToolResult, error) {
	// Fast rejection before the lock: after Close publishes the gate, new
	// calls must fail softly immediately instead of queueing behind the
	// drain (a writer-waiting RWMutex would block them for the whole drain).
	if t.lc.closing.Load() {
		return tool.NewErrorResult("MCP tool %q is shutting down", t.remoteName), nil
	}
	t.lc.mu.RLock()
	defer t.lc.mu.RUnlock()
	// Re-check under the lock: Close may have published the gate between the
	// fast check and admission, then be draining; this call must not proceed
	// into a session that Close is about to tear down.
	if t.lc.closing.Load() {
		return tool.NewErrorResult("MCP tool %q is shutting down", t.remoteName), nil
	}
	var args map[string]any
	if err := json.Unmarshal(raw, &args); err != nil || args == nil {
		return tool.NewErrorResult("MCP tool arguments must be a JSON object"), nil
	}
	result, err := t.session.CallTool(ctx, &sdk.CallToolParams{Name: t.remoteName, Arguments: args})
	if err != nil {
		return nil, fmt.Errorf("call MCP tool %q: %w", t.remoteName, err)
	}
	if result == nil {
		return nil, fmt.Errorf("call MCP tool %q: nil result", t.remoteName)
	}
	if result.NeedsInput() {
		return tool.NewErrorResult("v1 不支持 MCP input-required continuation"), nil
	}
	mapped, err := mapResult(result, t.dataLimit)
	if err != nil {
		var mapper *mapperError
		if errors.As(err, &mapper) {
			return tool.NewErrorResult("%v", err), nil
		}
		return nil, err
	}
	if result.IsError {
		mapped.Status = tool.ResultError
	}
	return mapped, nil
}
