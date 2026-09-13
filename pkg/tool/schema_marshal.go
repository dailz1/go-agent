package tool

import (
	"bytes"
	"encoding/json"
	"sort"
)

// MarshalJSON writes the canonical schema representation after structural validation.
func (s ParameterSchema) MarshalJSON() ([]byte, error) {
	if err := validateSchemaGraph(s); err != nil {
		return nil, err
	}
	return marshalParameter(s)
}

// MarshalJSON writes a schema node after structural validation.
func (p Property) MarshalJSON() ([]byte, error) {
	if err := validateSchemaGraph(ParameterSchema{Properties: map[string]Property{"property": p}}); err != nil {
		return nil, err
	}
	return marshalProperty(p)
}

func marshalParameter(s ParameterSchema) ([]byte, error) {
	fields := make([]schemaField, 0, 10+len(s.Keywords))
	if !s.typeAbsent {
		fields = append(fields, jsonField("type", s.Type))
	}
	if !s.propertiesInactive {
		properties, err := marshalProperties(s.Properties)
		if err != nil {
			return nil, err
		}
		fields = append(fields, rawField("properties", properties))
	}
	if len(s.Required) != 0 {
		fields = append(fields, jsonField("required", s.Required))
	}
	if s.Description != "" || s.descriptionPresent {
		fields = append(fields, jsonField("description", s.Description))
	}
	more, err := schemaFields(s.AdditionalProperties, s.AdditionalPropertiesSchema, s.AnyOf, s.OneOf, s.AllOf, s.Ref, s.refPresent)
	if err != nil {
		return nil, err
	}
	fields = append(fields, more...)
	return marshalObject(fields, s.Keywords)
}

func marshalProperty(p Property) ([]byte, error) {
	if p.BooleanSchema != nil {
		return json.Marshal(*p.BooleanSchema)
	}
	fields := make([]schemaField, 0, 12+len(p.Keywords))
	if p.typeActive() {
		if p.Nullable {
			fields = append(fields, jsonField("type", []string{"null", p.Type}))
		} else {
			fields = append(fields, jsonField("type", p.Type))
		}
	}
	if p.descriptionActive() {
		fields = append(fields, jsonField("description", p.Description))
	}
	if len(p.Enum) != 0 {
		fields = append(fields, jsonField("enum", p.Enum))
	}
	if len(p.Properties) != 0 {
		properties, err := marshalProperties(p.Properties)
		if err != nil {
			return nil, err
		}
		fields = append(fields, rawField("properties", properties))
	}
	if len(p.Required) != 0 {
		fields = append(fields, jsonField("required", p.Required))
	}
	if p.Items != nil {
		raw, err := marshalProperty(*p.Items)
		if err != nil {
			return nil, err
		}
		fields = append(fields, rawField("items", raw))
	}
	more, err := schemaFields(p.AdditionalProperties, p.AdditionalPropertiesSchema, p.AnyOf, p.OneOf, p.AllOf, p.Ref, p.refPresent)
	if err != nil {
		return nil, err
	}
	fields = append(fields, more...)
	return marshalObject(fields, p.Keywords)
}

type schemaField struct {
	name string
	raw  []byte
}

func jsonField(name string, value any) schemaField {
	raw, _ := json.Marshal(value)
	return rawField(name, raw)
}

func rawField(name string, raw []byte) schemaField { return schemaField{name: name, raw: raw} }

func schemaFields(ap *bool, aps *Property, anyOf, oneOf, allOf []*Property, ref string, refPresent bool) ([]schemaField, error) {
	var fields []schemaField
	if ap != nil {
		fields = append(fields, jsonField("additionalProperties", *ap))
	} else if aps != nil {
		raw, err := marshalProperty(*aps)
		if err != nil {
			return nil, err
		}
		fields = append(fields, rawField("additionalProperties", raw))
	}
	for _, pair := range []struct {
		name  string
		items []*Property
	}{{"anyOf", anyOf}, {"oneOf", oneOf}, {"allOf", allOf}} {
		if len(pair.items) != 0 {
			raw, err := marshalComposition(pair.items)
			if err != nil {
				return nil, err
			}
			fields = append(fields, rawField(pair.name, raw))
		}
	}
	if ref != "" || refPresent {
		fields = append(fields, jsonField("$ref", ref))
	}
	return fields, nil
}

func marshalComposition(items []*Property) ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('[')
	for i, item := range items {
		if i != 0 {
			b.WriteByte(',')
		}
		raw, err := marshalProperty(*item)
		if err != nil {
			return nil, err
		}
		b.Write(raw)
	}
	b.WriteByte(']')
	return b.Bytes(), nil
}

func marshalProperties(properties map[string]Property) ([]byte, error) {
	if properties == nil {
		return []byte("null"), nil
	}
	names := make([]string, 0, len(properties))
	for name := range properties {
		names = append(names, name)
	}
	sort.Strings(names)
	var b bytes.Buffer
	b.WriteByte('{')
	for i, name := range names {
		if i != 0 {
			b.WriteByte(',')
		}
		key, err := json.Marshal(name)
		if err != nil {
			return nil, err
		}
		raw, err := marshalProperty(properties[name])
		if err != nil {
			return nil, err
		}
		b.Write(key)
		b.WriteByte(':')
		b.Write(raw)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func marshalObject(fields []schemaField, keywords []SchemaKeyword) ([]byte, error) {
	keywords = append([]SchemaKeyword(nil), keywords...)
	sort.Slice(keywords, func(i, j int) bool { return keywords[i].Name < keywords[j].Name })
	var b bytes.Buffer
	b.WriteByte('{')
	first := true
	for _, field := range fields {
		if err := appendSchemaField(&b, &first, field.name, field.raw); err != nil {
			return nil, err
		}
	}
	for _, keyword := range keywords {
		if err := appendSchemaField(&b, &first, keyword.Name, keyword.Value); err != nil {
			return nil, err
		}
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func appendSchemaField(b *bytes.Buffer, first *bool, name string, raw []byte) error {
	if !*first {
		b.WriteByte(',')
	}
	*first = false
	key, err := json.Marshal(name)
	if err != nil {
		return err
	}
	b.Write(key)
	b.WriteByte(':')
	b.Write(raw)
	return nil
}
