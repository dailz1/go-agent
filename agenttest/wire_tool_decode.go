package agenttest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/dailz1/go-agent/tool"
)

func decodeWireTool(raw json.RawMessage, version int) (tool.ToolInfo, error) {
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
	if version == 1 {
		return decodeWireToolV1(out, object["parameters"])
	}
	parameters, err := wireRaw(object, "parameters")
	if err != nil {
		return out, err
	}
	if err := validateV2Parameters(parameters); err != nil {
		return out, incompatiblef("parameters: %v", err)
	}
	if err := out.Parameters.UnmarshalJSON(parameters); err != nil {
		return out, incompatiblef("parameters: %v", err)
	}
	return out, nil
}

// decodeWireToolV1 deliberately retains the frozen v1 schema grammar.
func decodeWireToolV1(out tool.ToolInfo, raw json.RawMessage) (tool.ToolInfo, error) {
	parameters, err := wireObject(raw, "type", "properties", "required")
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

// validateV2Parameters checks the local raw-recording boundary before the tool
// codec projects the schema and restores its private presence state.
func validateV2Parameters(raw json.RawMessage) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	token, err := dec.Token()
	if err != nil {
		return err
	}
	if token != json.Delim('{') {
		return fmt.Errorf("schema must be a JSON object")
	}
	if err := scanV2SchemaObject(dec); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("schema has trailing JSON")
		}
		return err
	}
	return nil
}

func scanV2SchemaValue(dec *json.Decoder) error {
	token, err := dec.Token()
	if err != nil {
		return err
	}
	switch token {
	case json.Delim('{'):
		return scanV2SchemaObject(dec)
	case json.Delim('['):
		for dec.More() {
			if err := scanV2SchemaValue(dec); err != nil {
				return err
			}
		}
		_, err := dec.Token()
		return err
	case json.Delim('}'), json.Delim(']'):
		return fmt.Errorf("unexpected JSON delimiter %q", token)
	default:
		return nil
	}
}

func scanV2SchemaObject(dec *json.Decoder) error {
	seen := map[string]bool{}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return err
		}
		name, ok := key.(string)
		if !ok {
			return fmt.Errorf("object key is not a string")
		}
		if seen[name] {
			return fmt.Errorf("duplicate JSON key %q", name)
		}
		seen[name] = true
		if err := scanV2SchemaValue(dec); err != nil {
			return err
		}
	}
	_, err := dec.Token()
	return err
}
