package codex

import (
	"context"
	"errors"
	"testing"
)

func TestErrorChain(t *testing.T) {
	err := &Error{Kind: KindAuth, Code: "cancelled", Cause: context.Canceled}
	var typed *Error
	if !errors.Is(err, context.Canceled) || !errors.As(err, &typed) || err.Error() == "" {
		t.Fatal("error lost its public classification or cause")
	}
	for _, kind := range []string{KindAuth, KindQuota, KindCapability, KindProtocol} {
		if (&Error{Kind: kind}).Error() == "" {
			t.Fatal("empty error")
		}
	}
}
