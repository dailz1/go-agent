package llm

import (
	"errors"
	"fmt"
	"time"
)

// ErrContextOverflow identifies a provider-confirmed context window overflow.
var ErrContextOverflow = errors.New("context window exceeded")

// ContextOverflowError preserves the protocol code and original provider error.
type ContextOverflowError struct {
	Code string
	Err  error
}

func (e *ContextOverflowError) Error() string {
	return fmt.Sprintf("context overflow (%s): %v", e.Code, e.Err)
}

func (e *ContextOverflowError) Unwrap() error { return e.Err }

func (e *ContextOverflowError) Is(target error) bool { return target == ErrContextOverflow }

// APIError represents an error returned by an LLM provider's HTTP API.
type APIError struct {
	StatusCode int
	Body       string
	RetryAfter time.Duration
	// NonRetryable overrides the status-code policy, for example for exhausted quota.
	NonRetryable bool
}

func (e *APIError) Error() string {
	return fmt.Sprintf("api error (status %d): %s", e.StatusCode, e.Body)
}

// Retryable reports whether the request can be retried.
// Retries are allowed on 429 (rate limit) and 5xx (server error) responses.
func (e *APIError) Retryable() bool {
	return !e.NonRetryable && (e.StatusCode == 429 || (e.StatusCode >= 500 && e.StatusCode < 600))
}
