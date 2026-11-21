// Package source owns the models.dev decoding and snapshot rules.
package source

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
)

const Codex = "openai-codex"

// CheckJSON rejects duplicate object keys and trailing JSON values before
// encoding/json can silently replace an earlier member.
func CheckJSON(data []byte) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if err := walk(d); err != nil {
		return fmt.Errorf("validate json: %w", err)
	}
	if _, err := d.Token(); err != io.EOF {
		if err != nil {
			return fmt.Errorf("read json tail: %w", err)
		}
		return fmt.Errorf("unexpected trailing json value")
	}
	return nil
}

func walk(d *json.Decoder) error {
	t, err := d.Token()
	if err != nil {
		return err
	}
	switch t {
	case json.Delim('{'):
		keys := make(map[string]bool)
		for d.More() {
			token, err := d.Token()
			if err != nil {
				return err
			}
			key, ok := token.(string)
			if !ok {
				return fmt.Errorf("object key is not a string")
			}
			if keys[key] {
				return fmt.Errorf("duplicate object key %q", key)
			}
			keys[key] = true
			if err := walk(d); err != nil {
				return err
			}
		}
		_, err = d.Token()
		return err
	case json.Delim('['):
		for d.More() {
			if err := walk(d); err != nil {
				return err
			}
		}
		_, err = d.Token()
		return err
	default:
		return nil
	}
}

func object(data json.RawMessage) (map[string]json.RawMessage, error) {
	if len(data) == 0 || bytes.TrimSpace(data)[0] != '{' {
		return nil, fmt.Errorf("expected object")
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("decode object: %w", err)
	}
	return result, nil
}

func identity(fields map[string]json.RawMessage, key string) error {
	if key == "" {
		return fmt.Errorf("empty identity key")
	}
	if raw, exists := fields["id"]; exists {
		var id string
		if err := json.Unmarshal(raw, &id); err != nil {
			return fmt.Errorf("decode id for %q: %w", key, err)
		}
		if id != key {
			return fmt.Errorf("id %q conflicts with key %q", id, key)
		}
	}
	return nil
}

// Decode trims one complete upstream response. Only v2's Codex provider
// participates in v2 structural checks; v1's Codex entry is never adopted.
func Decode(data []byte, codexOnly bool) (map[string]map[string]int, int, error) {
	if err := CheckJSON(data); err != nil {
		return nil, 0, err
	}
	providers, err := object(data)
	if err != nil {
		return nil, 0, err
	}
	windows := make(map[string]map[string]int)
	skipped, total := 0, 0
	for key, raw := range providers {
		if (key == Codex) != codexOnly {
			continue
		}
		p, err := object(raw)
		if err != nil {
			return nil, 0, fmt.Errorf("provider %q: %w", key, err)
		}
		if err := identity(p, key); err != nil {
			return nil, 0, err
		}
		models, err := object(p["models"])
		if err != nil {
			return nil, 0, fmt.Errorf("provider %q models: %w", key, err)
		}
		values := make(map[string]int)
		for name, model := range models {
			fields, err := object(model)
			if err != nil {
				return nil, 0, fmt.Errorf("model %q/%q: %w", key, name, err)
			}
			if err := identity(fields, name); err != nil {
				return nil, 0, err
			}
			n, err := window(fields["limit"])
			if err != nil {
				return nil, 0, fmt.Errorf("model %q/%q limit: %w", key, name, err)
			}
			if n == 0 {
				skipped++
				continue
			}
			values[name] = n
			total++
		}
		windows[key] = values
	}
	if total == 0 {
		return nil, 0, fmt.Errorf("source has no valid windows")
	}
	return windows, skipped, nil
}

func window(raw json.RawMessage) (int, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return 0, nil
	}
	fields, err := object(raw)
	if err != nil {
		return 0, err
	}
	// Parse the JSON number's integer syntax directly, without float rounding.
	n, err := strconv.ParseInt(string(fields["context"]), 10, 32)
	if err != nil || n <= 0 {
		return 0, nil
	}
	return int(n), nil
}
