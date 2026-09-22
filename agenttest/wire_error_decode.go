package agenttest

import (
	"bytes"
	"encoding/json"
	"time"
)

func decodeWireError(raw json.RawMessage, version int, parent string) (*dtoError, error) {
	fields := []string{"kind", "message", "status_code", "retry_after", "body", "errno"}
	if version == 3 {
		fields = append(fields, "non_retryable", "category", "code", "retry_at", "stage", "temporary", "login_required", "cause")
		if err := scanV2SchemaValue(json.NewDecoder(bytes.NewReader(raw))); err != nil {
			return nil, incompatiblef("error object: %v", err)
		}
	}
	object, err := wireObject(raw, fields...)
	if err != nil {
		return nil, err
	}
	var kind string
	if err := wireValue(object, "kind", &kind); err != nil {
		return nil, err
	}
	allowed := map[string][]string{
		"context_canceled": {"kind"}, "deadline_exceeded": {"kind"}, "streaming_not_supported": {"kind"},
		"api":             {"kind", "status_code", "retry_after", "body"},
		"network_timeout": {"kind", "message"}, "network_temporary": {"kind", "message"},
		"network_url": {"kind", "message"}, "network_errno": {"kind", "errno"}, "generic": {"kind", "message"},
	}
	if version == 3 {
		allowed["api"] = append(allowed["api"], "non_retryable")
		allowed["codex"] = []string{"kind", "category", "code", "retry_at", "cause"}
		allowed["codex_auth"] = []string{"kind", "stage", "code", "temporary", "login_required", "message", "cause"}
	}
	if !wireOnly(object, allowed[kind]) || !validErrorCause(parent, kind) {
		return nil, incompatiblef("invalid error type, field, or cause %q", kind)
	}
	if version == 3 {
		for _, field := range allowed[kind] {
			value, err := wireRaw(object, field)
			if err != nil {
				return nil, err
			}
			if bytes.Equal(value, []byte("null")) && field != "cause" && field != "retry_at" {
				return nil, incompatiblef("null %s", field)
			}
		}
	}
	out := &dtoError{Kind: kind}
	switch kind {
	case "api":
		err = wireValue(object, "status_code", &out.StatusCode)
		if err == nil {
			err = wireValue(object, "retry_after", &out.RetryAfter)
		}
		if err == nil {
			err = wireValue(object, "body", &out.Body)
		}
		if err == nil && version == 3 {
			err = wireValue(object, "non_retryable", &out.NonRetryable)
		}
	case "network_timeout", "network_temporary", "network_url", "generic":
		err = wireValue(object, "message", &out.Message)
	case "network_errno":
		err = wireValue(object, "errno", &out.Errno)
	case "codex":
		err = decodeWireCodex(object, out)
	case "codex_auth":
		err = decodeWireAuth(object, out)
	}
	if err != nil {
		return nil, err
	}
	if kind == "codex" || kind == "codex_auth" {
		if cause := object["cause"]; !bytes.Equal(cause, []byte("null")) {
			out.Cause, err = decodeWireError(cause, version, kind)
			if err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

func decodeWireCodex(object map[string]json.RawMessage, out *dtoError) error {
	if err := wireValue(object, "category", &out.Category); err != nil {
		return err
	}
	if !validCodexCategory(out.Category) {
		return incompatiblef("invalid Codex category %q", out.Category)
	}
	if err := wireValue(object, "code", &out.Code); err != nil {
		return err
	}
	if !bytes.Equal(object["retry_at"], []byte("null")) {
		var timestamp string
		if err := wireValue(object, "retry_at", &timestamp); err != nil {
			return err
		}
		value, err := time.Parse(time.RFC3339Nano, timestamp)
		if err != nil {
			return incompatiblef("retry_at: %v", err)
		}
		out.RetryAt = &value
	}
	return nil
}

func decodeWireAuth(object map[string]json.RawMessage, out *dtoError) error {
	if err := wireValue(object, "stage", &out.Stage); err != nil {
		return err
	}
	if !validAuthStage(out.Stage) {
		return incompatiblef("invalid auth stage %q", out.Stage)
	}
	if err := wireValue(object, "code", &out.Code); err != nil {
		return err
	}
	if err := wireValue(object, "temporary", &out.Temporary); err != nil {
		return err
	}
	if err := wireValue(object, "login_required", &out.LoginRequired); err != nil {
		return err
	}
	return wireValue(object, "message", &out.Message)
}
