package llm

import (
	"strings"
	"testing"
)

func TestAPIError_Error(t *testing.T) {
	t.Parallel()
	err := &APIError{StatusCode: 429, Body: "rate limited"}
	got := err.Error()
	if !strings.Contains(got, "429") || !strings.Contains(got, "rate limited") {
		t.Errorf("Error() = %q, want containing status 429 and body", got)
	}
}

func TestAPIError_Retryable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		code int
		want bool
	}{
		{429, true},
		{500, true},
		{502, true},
		{503, true},
		{599, true},
		{400, false},
		{401, false},
		{403, false},
		{404, false},
		{200, false},
		{301, false},
	}
	for _, tt := range tests {
		if got := (&APIError{StatusCode: tt.code}).Retryable(); got != tt.want {
			t.Errorf("Retryable(%d) = %v, want %v", tt.code, got, tt.want)
		}
	}
}
