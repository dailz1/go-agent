package agenttest

import (
	"fmt"
	"reflect"

	"github.com/dailz1/go-agent/llm/codex"
	"github.com/dailz1/go-agent/llm/codex/auth"
)

// recordError is called while the recorder mutex is held.
func (r *Recorder) recordError(err error) *dtoError {
	encoded, encodeErr := encodeError(err)
	if encodeErr != nil && r.unsupported == nil {
		r.unsupported = fmt.Errorf("record error: %w", encodeErr)
	}
	return encoded
}

func encodeError(err error) (*dtoError, error) {
	classified, branched, checkErr := checkErrorChain(err, nil)
	if checkErr != nil {
		return nil, checkErr
	}
	if classified && branched {
		return nil, fmt.Errorf("Codex error has multiple causes")
	}
	return encodeErrorCause(err, "")
}

// Inspect before errors.As/Is: those helpers do not terminate on cyclic chains.
func checkErrorChain(err error, ancestors []error) (classified, branched bool, checkErr error) {
	if err == nil {
		return false, false, nil
	}
	// Non-comparable errors can unwrap to themselves without an identity that
	// reflection can compare. Bound inspection before calling errors.As/Is.
	if len(ancestors) >= 64 {
		return false, false, fmt.Errorf("error chain exceeds inspection limit")
	}
	value := reflect.ValueOf(err)
	if value.Kind() == reflect.Pointer && value.IsNil() {
		return false, false, fmt.Errorf("nil error pointer %T", err)
	}
	for _, ancestor := range ancestors {
		if value.Comparable() && value.Equal(reflect.ValueOf(ancestor)) {
			return false, false, fmt.Errorf("cyclic error chain")
		}
	}
	switch err.(type) {
	case *codex.Error, *auth.Error:
		classified = true
	}
	if err == auth.ErrLoginRequired {
		classified = true
	}
	ancestors = append(ancestors, err)
	var children []error
	switch err := err.(type) {
	case interface{ Unwrap() error }:
		children = []error{err.Unwrap()}
	case interface{ Unwrap() []error }:
		children = err.Unwrap()
		branched = true
	}
	for _, child := range children {
		childClassified, childBranched, err := checkErrorChain(child, ancestors)
		if err != nil {
			return false, false, err
		}
		classified = classified || childClassified
		branched = branched || childBranched
	}
	return classified, branched, nil
}

func encodeErrorCause(err error, parent string) (*dtoError, error) {
	if err == nil {
		return nil, nil
	}
	var out *dtoError
	var cause error
	switch value := recordingWrapper(err).(type) {
	case *codex.Error:
		ce := value
		out = &dtoError{Kind: "codex", Category: ce.Kind, Code: ce.Code}
		if !validCodexCategory(ce.Kind) {
			return nil, fmt.Errorf("invalid Codex category %q", ce.Kind)
		}
		if ce.RetryAt != nil {
			value := *ce.RetryAt
			out.RetryAt = &value
		}
		cause = ce.Cause
	case *auth.Error:
		ae := value
		out = &dtoError{Kind: "codex_auth", Stage: ae.Stage, Code: ae.Code, Temporary: ae.Temporary, LoginRequired: ae.LoginRequired, Message: ae.Message}
		if !validAuthStage(ae.Stage) {
			return nil, fmt.Errorf("invalid auth stage %q", ae.Stage)
		}
		cause = ae.Cause
	default:
		if value != auth.ErrLoginRequired {
			return encodeLegacyError(err), nil
		}
		out = &dtoError{Kind: "codex_auth", Stage: "token", LoginRequired: true}
	}
	if !validErrorCause(parent, out.Kind) {
		return nil, fmt.Errorf("cannot record %s inside %s", out.Kind, parent)
	}
	var encodeErr error
	out.Cause, encodeErr = encodeErrorCause(cause, out.Kind)
	if encodeErr != nil {
		return nil, encodeErr
	}
	return out, nil
}

func recordingWrapper(err error) error {
	for err != nil {
		switch err.(type) {
		case *codex.Error, *auth.Error:
			return err
		}
		if err == auth.ErrLoginRequired {
			return err
		}
		next, ok := err.(interface{ Unwrap() error })
		if !ok {
			return nil
		}
		err = next.Unwrap()
	}
	return nil
}

func validCodexCategory(category string) bool {
	return category == "auth" || category == "quota" || category == "capability" || category == "protocol"
}

func validAuthStage(stage string) bool {
	return stage == "token" || stage == "refresh" || stage == "persist"
}

func validErrorCause(parent, kind string) bool {
	switch kind {
	case "codex":
		return parent == ""
	case "codex_auth":
		return parent == "" || parent == "codex"
	default:
		return true
	}
}
