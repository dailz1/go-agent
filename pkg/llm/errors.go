package llm

import "fmt"

// APIError represents an error returned by an LLM provider's HTTP API.
type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("api error (status %d): %s", e.StatusCode, e.Body)
}

// Retryable reports whether the request can be retried.
// Retries are allowed on 429 (rate limit) and 5xx (server error) responses.
func (e *APIError) Retryable() bool {
	return e.StatusCode == 429 || (e.StatusCode >= 500 && e.StatusCode < 600)
}
