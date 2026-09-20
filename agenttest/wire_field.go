package agenttest

import (
	"encoding/json"
)

func wireRaw(object map[string]json.RawMessage, key string) (json.RawMessage, error) {
	raw, ok := object[key]
	if !ok {
		return nil, incompatiblef("missing %s", key)
	}
	return raw, nil
}
