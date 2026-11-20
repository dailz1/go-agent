package llm

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestContextOverflowErrorChain(t *testing.T) {
	api := &APIError{StatusCode: 400, Body: "original body"}
	marker := &ContextOverflowError{Code: "context_length_exceeded", Err: api}
	tests := []struct {
		name string
		err  error
	}{
		{"direct", marker},
		{"wrapped", fmt.Errorf("provider request: %w", marker)},
		{"joined retryable", errors.Join(marker, &APIError{StatusCode: 503})},
		{"joined streaming unsupported", errors.Join(marker, ErrStreamingNotSupported)},
		{"joined cancellation", errors.Join(marker, context.Canceled)},
		// These contexts preserve classification; deciding whether to recover
		// belongs to the kernel's call boundary, not the error type.
		{"callback", fmt.Errorf("callback: %w", marker)},
		{"compactor", fmt.Errorf("compactor: %w", marker)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !errors.Is(tt.err, ErrContextOverflow) {
				t.Fatalf("marker lost: %v", tt.err)
			}
			var original *APIError
			var overflow *ContextOverflowError
			if !errors.As(tt.err, &original) || original != api || original.Retryable() {
				t.Fatalf("original API error lost or changed: %v", tt.err)
			}
			if !errors.As(tt.err, &overflow) || overflow.Code != "context_length_exceeded" {
				t.Fatalf("protocol code lost: %v", tt.err)
			}
			if errors.Unwrap(overflow) != api || overflow.Error() == "" {
				t.Fatal("marker did not preserve its cause")
			}
		})
	}
	if errors.Is(errors.New("context_length_exceeded"), ErrContextOverflow) {
		t.Fatal("message-only error classified")
	}
	if errors.Unwrap(api) != nil {
		t.Fatal("APIError must remain a leaf")
	}
}
