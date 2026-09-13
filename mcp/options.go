// Package mcpbridge bridges statically discovered MCP tools into a tool.Registry.
package mcpbridge

import "fmt"

const defaultResultDataLimit int64 = 1 << 20

// Config identifies the namespace used for final local tool names.
type Config struct {
	Namespace string
}

type config struct {
	approvalRequired bool
	resultDataLimit  int64
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
