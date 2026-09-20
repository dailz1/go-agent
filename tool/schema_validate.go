package tool

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
)

type schemaValidation struct {
	maps map[uintptr]string
	ptrs map[*Property]string
}

func validateSchemaGraph(schema ParameterSchema) error {
	v := schemaValidation{maps: map[uintptr]string{}, ptrs: map[*Property]string{}}
	return v.parameter(schema, "parameters")
}

func (v schemaValidation) parameter(s ParameterSchema, path string) error {
	if err := v.keywords(s.Keywords, parameterKeys(s), path); err != nil {
		return err
	}
	if s.AdditionalProperties != nil && s.AdditionalPropertiesSchema != nil {
		return fmt.Errorf("%s: additionalProperties has both bool and schema", path)
	}
	if err := v.properties(s.Properties, s.Required, path+".properties"); err != nil {
		return err
	}
	return v.children(nil, s.AdditionalPropertiesSchema, s.AnyOf, s.OneOf, s.AllOf, path)
}

func (v schemaValidation) property(p Property, path string) error {
	if p.BooleanSchema != nil {
		if p.booleanConflict() {
			return fmt.Errorf("%s: boolean schema has other fields", path)
		}
		return nil
	}
	if err := v.keywords(p.Keywords, propertyKeys(p), path); err != nil {
		return err
	}
	if p.AdditionalProperties != nil && p.AdditionalPropertiesSchema != nil {
		return fmt.Errorf("%s: additionalProperties has both bool and schema", path)
	}
	if err := v.properties(p.Properties, p.Required, path+".properties"); err != nil {
		return err
	}
	return v.children(p.Items, p.AdditionalPropertiesSchema, p.AnyOf, p.OneOf, p.AllOf, path)
}

func (v schemaValidation) properties(props map[string]Property, _ []string, path string) error {
	if props == nil {
		return nil
	}
	id := reflect.ValueOf(props).Pointer()
	if previous, ok := v.maps[id]; ok {
		return fmt.Errorf("%s: properties cycle to %s", path, previous)
	}
	v.maps[id] = path
	defer delete(v.maps, id)
	for name, p := range props {
		if err := v.property(p, path+"."+name); err != nil {
			return err
		}
	}
	return nil
}

func validateRegistrySchema(s ParameterSchema) error {
	if err := validateSchemaGraph(s); err != nil {
		return err
	}
	if s.propertiesInactive {
		return nil
	}
	return validateTypedProperties(s.Properties, s.Required, "parameters.properties")
}

func validateTypedProperties(props map[string]Property, required []string, path string) error {
	for _, name := range required {
		if _, ok := props[name]; !ok {
			return fmt.Errorf("%s: required parameter %q not defined in properties", path, name)
		}
	}
	for name, p := range props {
		if p.Type == "" && p.typeActive() && p.BooleanSchema == nil {
			return fmt.Errorf("%s.%s: property has empty type", path, name)
		}
		if err := validateNestedTypedProperties(p, path+"."+name); err != nil {
			return err
		}
	}
	return nil
}

func validateNestedTypedProperties(p Property, path string) error {
	if !p.propertiesInactive {
		if err := validateTypedProperties(p.Properties, p.Required, path+".properties"); err != nil {
			return err
		}
	}
	for _, edge := range []struct {
		name  string
		child *Property
	}{{"items", p.Items}, {"additionalProperties", p.AdditionalPropertiesSchema}} {
		if edge.child != nil {
			if err := validateNestedTypedProperties(*edge.child, path+"."+edge.name); err != nil {
				return err
			}
		}
	}
	for _, group := range []struct {
		name  string
		items []*Property
	}{{"anyOf", p.AnyOf}, {"oneOf", p.OneOf}, {"allOf", p.AllOf}} {
		for i, child := range group.items {
			if err := validateNestedTypedProperties(*child, fmt.Sprintf("%s.%s[%d]", path, group.name, i)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (v schemaValidation) children(items, ap *Property, anyOf, oneOf, allOf []*Property, path string) error {
	for _, edge := range []struct {
		name  string
		child *Property
	}{{"items", items}, {"additionalProperties", ap}} {
		if edge.child != nil {
			if err := v.pointer(edge.child, path+"."+edge.name); err != nil {
				return err
			}
		}
	}
	for _, group := range []struct {
		name  string
		items []*Property
	}{{"anyOf", anyOf}, {"oneOf", oneOf}, {"allOf", allOf}} {
		for i, child := range group.items {
			childPath := fmt.Sprintf("%s.%s[%d]", path, group.name, i)
			if child == nil {
				return fmt.Errorf("%s: composition contains nil schema", childPath)
			}
			if err := v.pointer(child, childPath); err != nil {
				return err
			}
		}
	}
	return nil
}

func (v schemaValidation) pointer(p *Property, path string) error {
	if previous, ok := v.ptrs[p]; ok {
		return fmt.Errorf("%s: property cycle to %s", path, previous)
	}
	v.ptrs[p] = path
	defer delete(v.ptrs, p)
	return v.property(*p, path)
}

func (v schemaValidation) keywords(keywords []SchemaKeyword, active map[string]bool, path string) error {
	seen := make(map[string]bool, len(keywords))
	for _, keyword := range keywords {
		if seen[keyword.Name] {
			return fmt.Errorf("%s: duplicate keyword %q", path, keyword.Name)
		}
		if active[keyword.Name] {
			return fmt.Errorf("%s: keyword %q conflicts with typed field", path, keyword.Name)
		}
		if err := validJSONValue(keyword.Value); err != nil {
			return fmt.Errorf("%s: keyword %q: %w", path, keyword.Name, err)
		}
		seen[keyword.Name] = true
	}
	return nil
}

func parameterKeys(s ParameterSchema) map[string]bool {
	return map[string]bool{"type": !s.typeAbsent, "properties": !s.propertiesInactive, "required": len(s.Required) != 0, "description": s.Description != "" || s.descriptionPresent, "additionalProperties": s.AdditionalProperties != nil || s.AdditionalPropertiesSchema != nil, "anyOf": len(s.AnyOf) != 0, "oneOf": len(s.OneOf) != 0, "allOf": len(s.AllOf) != 0, "$ref": s.Ref != "" || s.refPresent}
}

func propertyKeys(p Property) map[string]bool {
	return map[string]bool{"type": p.typeActive(), "description": p.descriptionActive(), "enum": len(p.Enum) != 0, "properties": !p.propertiesInactive && len(p.Properties) != 0, "required": len(p.Required) != 0, "items": p.Items != nil, "additionalProperties": p.AdditionalProperties != nil || p.AdditionalPropertiesSchema != nil, "anyOf": len(p.AnyOf) != 0, "oneOf": len(p.OneOf) != 0, "allOf": len(p.AllOf) != 0, "$ref": p.Ref != "" || p.refPresent}
}

func validJSONValue(raw json.RawMessage) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := scanJSONValue(dec); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}

func scanJSONValue(dec *json.Decoder) error {
	token, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for dec.More() {
			key, err := dec.Token()
			if err != nil {
				return err
			}
			name := key.(string)
			if seen[name] {
				return fmt.Errorf("duplicate JSON key %q", name)
			}
			seen[name] = true
			if err := scanJSONValue(dec); err != nil {
				return err
			}
		}
	case '[':
		for dec.More() {
			if err := scanJSONValue(dec); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
	_, err = dec.Token()
	return err
}
