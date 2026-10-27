package skills

import (
	"context"
	"encoding/json"

	"github.com/dailz1/go-agent/tool"
)

var _ tool.Tool = (*distributor)(nil)

// distributor is a Set's single tool: Description carries the catalog;
// Execute resolves a name against the immutable set. Both are pure
// in-memory reads, so Execute never returns a Go error, Data stays nil,
// and RequiresApproval is fixed false.
type distributor struct {
	set *Set
}

func (d *distributor) Info() tool.ToolInfo {
	parameters := tool.NewParameterSchema()
	parameters.Properties["name"] = tool.Param("string", "name of the skill to load")
	parameters.Required = []string{"name"}
	return tool.ToolInfo{
		Name:        d.set.toolName,
		Description: d.set.catalog,
		Parameters:  parameters,
	}
}

// Execute answers with the named skill's body verbatim. Malformed arguments
// and unknown names are soft errors the model can recover from — the model
// re-reads the catalog it holds and retries; the run never stops.
func (d *distributor) Execute(ctx context.Context, args json.RawMessage) (*tool.ToolResult, error) {
	var call struct {
		Name *string `json:"name"`
	}
	if err := json.Unmarshal(args, &call); err != nil || call.Name == nil {
		return tool.NewErrorResult("invalid arguments: expected object with string name"), nil
	}
	body, ok := d.set.byName[*call.Name]
	if !ok {
		return tool.NewErrorResult("skill %q not found", *call.Name), nil
	}
	return tool.NewTextResult(body), nil
}
