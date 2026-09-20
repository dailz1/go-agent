package mcpbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/dailz1/go-agent/tool"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Bridge owns one MCP client session. The registry passed to Connect must be
// fresh, unpublished, and quiescent until Connect returns successfully.
type Bridge struct {
	session *sdk.ClientSession
	names   []string
}

// Connect discovers every paginated MCP tool and registers static wrappers.
// It does not refresh tools. Callers must cancel and join every Execute before
// Close; Close concurrent with Execute is unsupported.
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
	closeOnError := true
	defer func() {
		if closeOnError {
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
		if err := registry.Register(wrapper); err != nil {
			return nil, fmt.Errorf("register MCP tool %q: %w", wrapper.info.Name, err)
		}
	}
	names := make([]string, len(wrappers))
	for i, wrapper := range wrappers {
		names[i] = wrapper.info.Name
	}
	sort.Strings(names)
	closeOnError = false
	return &Bridge{session: session, names: names}, nil
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

// Names returns sorted copies of final registered names.
func (b *Bridge) Names() []string {
	if b == nil {
		return nil
	}
	return append([]string(nil), b.names...)
}

// Close delegates to the SDK session. The caller must first cancel and join
// all in-flight Execute calls; Close concurrent with Execute is unsupported.
func (b *Bridge) Close() error {
	if b == nil || b.session == nil {
		return nil
	}
	return b.session.Close()
}

type remoteTool struct {
	session    *sdk.ClientSession
	remoteName string
	info       tool.ToolInfo
	dataLimit  int64
}

func (t *remoteTool) Info() tool.ToolInfo { return t.info }

func (t *remoteTool) Execute(ctx context.Context, raw json.RawMessage) (*tool.ToolResult, error) {
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
