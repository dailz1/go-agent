package tool

import (
	"bytes"
	"encoding/json"
)

// UnmarshalJSON resets and projects one object-shaped parameter schema.
func (s *ParameterSchema) UnmarshalJSON(data []byte) error {
	*s = ParameterSchema{typeAbsent: true}
	members, err := schemaObject(data)
	if err != nil {
		return err
	}
	s.Properties = map[string]Property{}
	for _, member := range members {
		switch member.name {
		case "type":
			if typ, nullable, ok := schemaType(member.value); ok && !nullable {
				s.Type, s.typeAbsent = typ, false
			} else {
				s.Keywords = append(s.Keywords, member.keyword())
			}
		case "properties":
			properties, null, ok := schemaProperties(member.value)
			if !ok {
				s.Properties = nil
				s.propertiesInactive = true
				s.Keywords = append(s.Keywords, member.keyword())
			} else if null {
				s.Properties = nil
			} else {
				s.Properties = properties
			}
		case "required":
			if values, ok := stringSlice(member.value); ok {
				s.Required = values
			} else {
				s.Keywords = append(s.Keywords, member.keyword())
			}
		case "description":
			if description, ok := jsonString(member.value); ok {
				s.Description, s.descriptionPresent = description, true
			} else {
				s.Keywords = append(s.Keywords, member.keyword())
			}
		case "additionalProperties":
			if err := decodeAdditional(member.value, &s.AdditionalProperties, &s.AdditionalPropertiesSchema); err != nil {
				s.Keywords = append(s.Keywords, member.keyword())
			}
		case "anyOf", "oneOf", "allOf":
			if values, ok := propertySlice(member.value); ok {
				switch member.name {
				case "anyOf":
					s.AnyOf = values
				case "oneOf":
					s.OneOf = values
				case "allOf":
					s.AllOf = values
				}
			} else {
				s.Keywords = append(s.Keywords, member.keyword())
			}
		case "$ref":
			if ref, ok := jsonString(member.value); ok {
				s.Ref, s.refPresent = ref, true
			} else {
				s.Keywords = append(s.Keywords, member.keyword())
			}
		default:
			s.Keywords = append(s.Keywords, member.keyword())
		}
	}
	return nil
}

// UnmarshalJSON resets and projects an object or bare boolean schema node.
func (p *Property) UnmarshalJSON(data []byte) error {
	*p = Property{decoded: true}
	var boolean bool
	if json.Unmarshal(data, &boolean) == nil && (bytes.Equal(bytes.TrimSpace(data), []byte("true")) || bytes.Equal(bytes.TrimSpace(data), []byte("false"))) {
		p.BooleanSchema = &boolean
		p.typeAbsent, p.descriptionAbsent = true, true
		return nil
	}
	members, err := schemaObject(data)
	if err != nil {
		return err
	}
	p.typeAbsent, p.descriptionAbsent = true, true
	for _, member := range members {
		switch member.name {
		case "type":
			if typ, nullable, ok := schemaType(member.value); ok {
				p.Type, p.Nullable, p.typeAbsent = typ, nullable, false
			} else {
				p.Keywords = append(p.Keywords, member.keyword())
			}
		case "description":
			if description, ok := jsonString(member.value); ok {
				p.Description, p.descriptionAbsent = description, false
			} else {
				p.Keywords = append(p.Keywords, member.keyword())
			}
		case "enum":
			if values, ok := stringSlice(member.value); ok && len(values) != 0 {
				p.Enum = values
			} else {
				p.Keywords = append(p.Keywords, member.keyword())
			}
		case "properties":
			if values, null, ok := schemaProperties(member.value); ok && !null {
				if len(values) != 0 {
					p.Properties = values
				}
			} else {
				p.propertiesInactive = true
				p.Keywords = append(p.Keywords, member.keyword())
			}
		case "required":
			if values, ok := stringSlice(member.value); ok {
				p.Required = values
			} else {
				p.Keywords = append(p.Keywords, member.keyword())
			}
		case "items":
			if value, ok := property(member.value); ok {
				p.Items = value
			} else {
				p.Keywords = append(p.Keywords, member.keyword())
			}
		case "additionalProperties":
			if err := decodeAdditional(member.value, &p.AdditionalProperties, &p.AdditionalPropertiesSchema); err != nil {
				p.Keywords = append(p.Keywords, member.keyword())
			}
		case "anyOf", "oneOf", "allOf":
			if values, ok := propertySlice(member.value); ok {
				switch member.name {
				case "anyOf":
					p.AnyOf = values
				case "oneOf":
					p.OneOf = values
				case "allOf":
					p.AllOf = values
				}
			} else {
				p.Keywords = append(p.Keywords, member.keyword())
			}
		case "$ref":
			if ref, ok := jsonString(member.value); ok {
				p.Ref, p.refPresent = ref, true
			} else {
				p.Keywords = append(p.Keywords, member.keyword())
			}
		default:
			p.Keywords = append(p.Keywords, member.keyword())
		}
	}
	return nil
}
