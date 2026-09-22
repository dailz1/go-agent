package codex

import (
	"fmt"
	"time"
)

// Error kinds distinguish subscription failures without inspecting error text.
const (
	KindAuth       = "auth"
	KindQuota      = "quota"
	KindCapability = "capability"
	KindProtocol   = "protocol"
)

// Error classifies a Codex failure while preserving its underlying cause.
// RetryAt is present only when the server supplied a parseable reset time.
type Error struct {
	Kind    string
	Code    string
	RetryAt *time.Time
	Cause   error
}

func (e *Error) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("codex %s (%s): %v", e.Kind, e.Code, e.Cause)
	}
	return fmt.Sprintf("codex %s (%s)", e.Kind, e.Code)
}

func (e *Error) Unwrap() error { return e.Cause }

func protocolError(format string, args ...any) error {
	return &Error{Kind: KindProtocol, Cause: fmt.Errorf(format, args...)}
}

func capabilityError(code string) error {
	return &Error{Kind: KindCapability, Code: code}
}
