package agenttest

import (
	"encoding/json"

	"github.com/dailz1/go-agent/pkg/tool"
)

func decodeWireTool(raw json.RawMessage) (tool.ToolInfo, error) {
	object, err := wireObject(raw, "name", "description", "parameters", "requires_approval")
	if err != nil {
		return tool.ToolInfo{}, err
	}
	var out tool.ToolInfo
	if err = wireValue(object, "name", &out.Name); err != nil {
		return out, err
	}
	if err = wireValue(object, "description", &out.Description); err != nil {
		return out, err
	}
	if err = wireOptional(object, "requires_approval", &out.RequiresApproval); err != nil {
		return out, err
	}
	parameters, err := wireObject(object["parameters"], "type", "properties", "required")
	if err != nil {
		return out, err
	}
	if err = wireValue(parameters, "type", &out.Parameters.Type); err != nil {
		return out, err
	}
	if err = wireOptional(parameters, "required", &out.Parameters.Required); err != nil {
		return out, err
	}
	var properties map[string]json.RawMessage
	if err = wireValue(parameters, "properties", &properties); err != nil {
		return out, err
	}
	if properties != nil {
		out.Parameters.Properties = make(map[string]tool.Property, len(properties))
		for name, property := range properties {
			fields, err := wireObject(property, "type", "description", "enum")
			if err != nil {
				return out, err
			}
			var value tool.Property
			if err = wireValue(fields, "type", &value.Type); err != nil {
				return out, err
			}
			if err = wireValue(fields, "description", &value.Description); err != nil {
				return out, err
			}
			if err = wireOptional(fields, "enum", &value.Enum); err != nil {
				return out, err
			}
			out.Parameters.Properties[name] = value
		}
	}
	return out, nil
}
