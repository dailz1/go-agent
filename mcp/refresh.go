package mcpbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/dailz1/go-agent/tool"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// RefreshSummary reports what one successful Refresh landed. Every list is
// sorted; empty categories stay nil. Conflicts names remote tools whose
// projected name is registered to someone else (a user tool, or a name the
// user took over with Replace): the bridge never touches them, and the rest
// of the batch still landed.
type RefreshSummary struct {
	Added     []string
	Removed   []string
	Replaced  []string
	Conflicts []string
}

// Refresh re-lists every remote tool and reconciles the bridge's owned set:
// new remote tools are registered, vanished ones unregistered, and changed
// ones replaced, all through one atomic Registry.Apply. The owned set is
// rewritten only after Apply succeeds; a fetch, representability, or apply
// failure leaves both the registry and the owned set unchanged. During
// Connect and Refresh no concurrent external registry mutation may touch the
// same namespace.
func (b *Bridge) Refresh(ctx context.Context) (*RefreshSummary, error) {
	if b == nil {
		return nil, fmt.Errorf("mcp bridge is nil")
	}
	remote, err := listTools(ctx, b.session)
	if err != nil {
		return nil, fmt.Errorf("list MCP tools: %w", err)
	}
	fresh, err := projectRemoteTools(remote, b.namespace, b.approvalRequired, b.dataLimit, b.session)
	if err != nil {
		return nil, err
	}

	summary := &RefreshSummary{}
	var removed []string
	var added []tool.Tool
	next := make(map[string]*remoteTool, len(fresh))
	for _, name := range sortedWrapperNames(fresh) {
		wrapper := fresh[name]
		ownedWrapper, wasOwned := b.owned[name]
		current, registered := b.registry.Get(name)
		stillOwned := registered && current == tool.Tool(ownedWrapper)
		switch {
		case !wasOwned && !registered:
			added = append(added, wrapper)
			summary.Added = append(summary.Added, name)
			next[name] = wrapper
		case !wasOwned:
			summary.Conflicts = append(summary.Conflicts, name)
		case !registered:
			added = append(added, wrapper)
			summary.Replaced = append(summary.Replaced, name)
			next[name] = wrapper
		case stillOwned && toolInfoEquivalent(ownedWrapper.info, wrapper.info):
			next[name] = ownedWrapper
		case stillOwned:
			removed = append(removed, name)
			added = append(added, wrapper)
			summary.Replaced = append(summary.Replaced, name)
			next[name] = wrapper
		default:
			summary.Conflicts = append(summary.Conflicts, name)
		}
	}
	for _, name := range sortedWrapperNames(b.owned) {
		if _, listed := fresh[name]; listed {
			continue
		}
		if current, ok := b.registry.Get(name); ok && current == tool.Tool(b.owned[name]) {
			removed = append(removed, name)
			summary.Removed = append(summary.Removed, name)
		}
	}

	if err := b.registry.Apply(removed, added); err != nil {
		return nil, fmt.Errorf("apply MCP tool changes: %w", err)
	}
	b.owned = next
	return summary, nil
}

// projectRemoteTools builds one wrapper per representable remote tool keyed
// by its projected name. Unrepresentable names, duplicate projected names,
// and unprojectable schemas fail the whole set, mirroring Connect.
func projectRemoteTools(remote []*sdk.Tool, namespace string, approvalRequired bool, dataLimit int64, session *sdk.ClientSession) (map[string]*remoteTool, error) {
	var errs []error
	fresh := make(map[string]*remoteTool, len(remote))
	for _, item := range remote {
		remoteName := ""
		if item != nil {
			remoteName = item.Name
		}
		name := namespace + "__" + remoteName
		if _, err := finalName(namespace, remoteName); err != nil {
			errs = append(errs, err)
		}
		if _, duplicate := fresh[name]; duplicate {
			errs = append(errs, &NameNotRepresentableError{Remote: remoteName, Final: name, Reason: "duplicate final name"})
		}
		schema, err := projectSchema(item)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if finalNamePattern.MatchString(name) {
			wrapper := newRemoteTool(item, name, schema, approvalRequired)
			wrapper.dataLimit = dataLimit
			wrapper.session = session
			fresh[name] = wrapper
		}
	}
	if err := sortedErrors(errs); err != nil {
		return nil, err
	}
	return fresh, nil
}

func sortedWrapperNames(wrappers map[string]*remoteTool) []string {
	names := make([]string, 0, len(wrappers))
	for name := range wrappers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func toolInfoEquivalent(a, b tool.ToolInfo) bool {
	if a.Description != b.Description || a.RequiresApproval != b.RequiresApproval {
		return false
	}
	left, leftErr := json.Marshal(a.Parameters)
	right, rightErr := json.Marshal(b.Parameters)
	return leftErr == nil && rightErr == nil && bytes.Equal(left, right)
}
