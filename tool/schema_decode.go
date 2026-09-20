package tool

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

type schemaMember struct {
	name  string
	value json.RawMessage
}

func (m schemaMember) keyword() SchemaKeyword {
	return SchemaKeyword{Name: m.name, Value: append(json.RawMessage(nil), m.value...)}
}

func schemaObject(data []byte) ([]schemaMember, error) {
	if err := validJSONValue(data); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	token, err := dec.Token()
	if err != nil || token != json.Delim('{') {
		return nil, fmt.Errorf("schema must be a JSON object")
	}
	var members []schemaMember
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return nil, err
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		members = append(members, schemaMember{name: key.(string), value: append(json.RawMessage(nil), raw...)})
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("schema has trailing JSON")
	}
	return members, nil
}

func schemaType(raw json.RawMessage) (string, bool, bool) {
	if jsonKind(raw, '"') {
		var typ string
		if json.Unmarshal(raw, &typ) == nil {
			return typ, false, true
		}
	}
	if !jsonKind(raw, '[') {
		return "", false, false
	}
	var rawTypes []json.RawMessage
	if json.Unmarshal(raw, &rawTypes) != nil || len(rawTypes) != 2 {
		return "", false, false
	}
	first, firstOK := jsonString(rawTypes[0])
	second, secondOK := jsonString(rawTypes[1])
	if !firstOK || !secondOK {
		return "", false, false
	}
	if first == "null" {
		return second, true, true
	}
	if second == "null" {
		return first, true, true
	}
	return "", false, false
}

func jsonString(raw json.RawMessage) (string, bool) {
	if !jsonKind(raw, '"') {
		return "", false
	}
	var value string
	return value, json.Unmarshal(raw, &value) == nil
}

func stringSlice(raw json.RawMessage) ([]string, bool) {
	if !jsonKind(raw, '[') {
		return nil, false
	}
	var rawValues []json.RawMessage
	if json.Unmarshal(raw, &rawValues) != nil {
		return nil, false
	}
	values := make([]string, len(rawValues))
	for i, rawValue := range rawValues {
		value, ok := jsonString(rawValue)
		if !ok {
			return nil, false
		}
		values[i] = value
	}
	return values, true
}

func schemaProperties(raw json.RawMessage) (map[string]Property, bool, bool) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, true, true
	}
	if !jsonKind(raw, '{') {
		return nil, false, false
	}
	members, err := schemaObject(raw)
	if err != nil {
		return nil, false, false
	}
	properties := make(map[string]Property, len(members))
	for _, member := range members {
		var p Property
		if err := json.Unmarshal(member.value, &p); err != nil {
			return nil, false, false
		}
		properties[member.name] = p
	}
	return properties, false, true
}

func property(raw json.RawMessage) (*Property, bool) {
	var p Property
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, false
	}
	return &p, true
}

func propertySlice(raw json.RawMessage) ([]*Property, bool) {
	if !jsonKind(raw, '[') {
		return nil, false
	}
	var values []json.RawMessage
	if json.Unmarshal(raw, &values) != nil || len(values) == 0 {
		return nil, false
	}
	result := make([]*Property, len(values))
	for i, raw := range values {
		value, ok := property(raw)
		if !ok {
			return nil, false
		}
		result[i] = value
	}
	return result, true
}

func jsonKind(raw json.RawMessage, kind byte) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) != 0 && raw[0] == kind
}

func decodeAdditional(raw json.RawMessage, target **bool, schema **Property) error {
	var value bool
	if json.Unmarshal(raw, &value) == nil && (bytes.Equal(bytes.TrimSpace(raw), []byte("true")) || bytes.Equal(bytes.TrimSpace(raw), []byte("false"))) {
		*target = &value
		return nil
	}
	valueSchema, ok := property(raw)
	if !ok {
		return fmt.Errorf("not an additional-properties schema")
	}
	*schema = valueSchema
	return nil
}
