package mcpbridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"

	"github.com/dailz1/go-agent/pkg/tool"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

var finalNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// NameNotRepresentableError reports a remote or final name that cannot be
// advertised to the provider-facing tool API.
type NameNotRepresentableError struct {
	Remote string
	Final  string
	Reason string
}

func (e *NameNotRepresentableError) Error() string {
	return fmt.Sprintf("MCP tool %q name %q is not representable: %s", e.Remote, e.Final, e.Reason)
}

// SchemaNotRepresentableError reports a decoded MCP input schema that cannot
// be projected by tool.ParameterSchema.
type SchemaNotRepresentableError struct {
	Remote string
	Err    error
}

func (e *SchemaNotRepresentableError) Error() string {
	return fmt.Sprintf("MCP tool %q schema is not representable: %v", e.Remote, e.Err)
}

func (e *SchemaNotRepresentableError) Unwrap() error { return e.Err }

func validNamespace(namespace string) bool {
	return finalNamePattern.MatchString(namespace) && !containsDoubleUnderscore(namespace)
}

func containsDoubleUnderscore(s string) bool {
	for i := 0; i+1 < len(s); i++ {
		if s[i:i+2] == "__" {
			return true
		}
	}
	return false
}

func finalName(namespace, remote string) (string, error) {
	name := namespace + "__" + remote
	if !finalNamePattern.MatchString(name) {
		return name, &NameNotRepresentableError{Remote: remote, Final: name, Reason: "must match ^[A-Za-z0-9_-]{1,64}$"}
	}
	return name, nil
}

func projectSchema(remote *sdk.Tool) (tool.ParameterSchema, error) {
	if remote == nil {
		return tool.ParameterSchema{}, &SchemaNotRepresentableError{Err: fmt.Errorf("nil tool")}
	}
	input, ok := remote.InputSchema.(map[string]any)
	if !ok {
		return tool.ParameterSchema{}, &SchemaNotRepresentableError{Remote: remote.Name, Err: fmt.Errorf("decoded input schema is not an object")}
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return tool.ParameterSchema{}, &SchemaNotRepresentableError{Remote: remote.Name, Err: fmt.Errorf("marshal decoded input schema: %w", err)}
	}
	var schema tool.ParameterSchema
	if err := schema.UnmarshalJSON(raw); err != nil {
		return tool.ParameterSchema{}, &SchemaNotRepresentableError{Remote: remote.Name, Err: err}
	}
	return schema, nil
}

func newRemoteTool(remote *sdk.Tool, name string, schema tool.ParameterSchema, approvalRequired bool) *remoteTool {
	return &remoteTool{remoteName: remote.Name, info: tool.ToolInfo{
		Name: name, Description: remote.Description, Parameters: schema,
		RequiresApproval: requiresApproval(remote.Annotations, approvalRequired),
	}}
}

func requiresApproval(a *sdk.ToolAnnotations, required bool) bool {
	if required || a == nil || !a.ReadOnlyHint {
		return true
	}
	return a.DestructiveHint != nil && *a.DestructiveHint
}

func sortedErrors(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	sort.Slice(errs, func(i, j int) bool { return errs[i].Error() < errs[j].Error() })
	return fmt.Errorf("MCP tools are not representable: %w", errors.Join(errs...))
}
