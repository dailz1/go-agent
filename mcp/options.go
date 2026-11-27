// Package mcpbridge bridges statically discovered MCP tools into a tool.Registry.
package mcpbridge

import (
	"fmt"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const defaultResultDataLimit int64 = 1 << 20

// Config identifies the namespace used for final local tool names.
type Config struct {
	Namespace string
}

type config struct {
	approvalRequired bool
	resultDataLimit  int64
	toolFilter       ToolFilter
}

// Option configures a Bridge.
type Option func(*config)

// WithApprovalRequired controls the approval baseline. It defaults to true.
func WithApprovalRequired(required bool) Option {
	return func(c *config) { c.approvalRequired = required }
}

// WithResultDataLimit sets the maximum encoded ToolResult.Data envelope size.
// bytes must be positive; invalid values are rejected by Connect.
func WithResultDataLimit(bytes int) Option {
	return func(c *config) { c.resultDataLimit = int64(bytes) }
}

// ToolFilter reports whether a remote MCP tool should be registered into the
// local registry. It receives a deep copy of the remote tool as discovered
// by tools/list; treat the input as read-only — mutating it has undefined
// effects on what gets registered. The final local name is Namespace +
// "__" + Tool.Name. Return false to skip the tool: it never reaches conflict
// checks or schema projection, but required name-validation errors are
// still recorded. Once a tool is rejected, its name stays excluded for the
// lifetime of the Bridge, even if the server later changes the tool.
type ToolFilter func(tool *sdk.Tool) bool

// WithToolFilter sets the connection-level tool filter. The filter applies
// at Connect and again at every Refresh, and rejections are retained: a name
// rejected at any listing stays out for the lifetime of the Bridge, even if
// a server-side change would later make the predicate accept it. A nil
// filter (the default) keeps current behavior: every discovered tool is
// registered. When used through Manager, the predicate runs while the
// manager holds its own lock; it must not call back into the same Manager
// (Snapshot, OwnerOf, Refresh) or use precomputed state instead.
func WithToolFilter(filter ToolFilter) Option {
	return func(c *config) { c.toolFilter = filter }
}

func resolveConfig(opts []Option) (config, error) {
	c := config{approvalRequired: true, resultDataLimit: defaultResultDataLimit}
	for _, opt := range opts {
		if opt != nil {
			opt(&c)
		}
	}
	if c.resultDataLimit <= 0 {
		return config{}, fmt.Errorf("mcp result data limit must be positive")
	}
	return c, nil
}
