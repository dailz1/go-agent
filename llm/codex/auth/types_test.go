package auth

import (
	"context"
	"errors"
	"testing"
)

func TestErrorClassification(t *testing.T) {
	cause := context.Canceled
	err := &Error{
		Stage: "refresh", Code: "invalid_grant", LoginRequired: true,
		Message: "authorization expired", Cause: cause,
	}
	if !errors.Is(err, ErrLoginRequired) || !errors.Is(err, cause) {
		t.Fatalf("classification lost: %v", err)
	}
	var got *Error
	if !errors.As(err, &got) || got != err || err.Error() == "" {
		t.Fatal("error type or message lost")
	}
	if errors.Is(&Error{Stage: "persist", Temporary: true}, ErrLoginRequired) {
		t.Fatal("persistence error requires login")
	}
}
