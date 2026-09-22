package codex

import (
	"encoding/json"
	"time"

	"github.com/dailz1/go-agent/llm"
)

func classifyServerError(server *serverError, api *llm.APIError) error {
	code := server.Code
	if code == "" {
		code = server.Type
	}
	kind := KindProtocol
	switch code {
	case "usage_limit_reached", "insufficient_quota", "quota_exceeded":
		kind = KindQuota
		if api != nil {
			api.NonRetryable = true
		}
	}
	if api != nil && kind != KindQuota {
		switch api.StatusCode {
		case 401:
			kind = KindAuth
		case 400, 403, 404, 422:
			kind = KindCapability
		default:
			return api
		}
	}
	var cause error
	if api != nil {
		cause = api
	}
	return &Error{Kind: kind, Code: code, RetryAt: resetTime(server), Cause: cause}
}

func resetTime(server *serverError) *time.Time {
	raw := server.ResetAt
	if len(raw) == 0 {
		raw = server.ResetsAt
	}
	var seconds int64
	if json.Unmarshal(raw, &seconds) == nil && seconds > 0 {
		at := time.Unix(seconds, 0)
		return &at
	}
	var value string
	if json.Unmarshal(raw, &value) == nil {
		if at, err := time.Parse(time.RFC3339Nano, value); err == nil {
			return &at
		}
	}
	return nil
}
