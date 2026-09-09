package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const (
	// MaxResponseBody caps the number of bytes read from an HTTP response body
	// to prevent OOM when a provider returns an unexpectedly large payload.
	// Used by [DoJSONRequest] and the OpenAI adapter's error handling.
	MaxResponseBody = 10 << 20 // 10 MB
)

// RequestConfig describes an HTTP request to an LLM provider API.
type RequestConfig struct {
	Method  string
	URL     string
	Headers map[string]string
}

// DoJSONRequest sends a JSON HTTP request and unmarshals the response into respBody.
// Non-2xx responses are returned as [*APIError]. Network errors are returned wrapped.
// The response body is capped at 10 MB to prevent OOM.
func DoJSONRequest(ctx context.Context, client *http.Client, cfg RequestConfig, reqBody, respBody any) error {
	body, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, cfg.Method, cfg.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range cfg.Headers {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("http request: %w", err)
	}

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBody))
	resp.Body.Close()
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &APIError{
			StatusCode: resp.StatusCode,
			Body:       string(respBytes),
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
		}
	}

	if err := json.Unmarshal(respBytes, respBody); err != nil {
		return fmt.Errorf("unmarshal response: %w", err)
	}
	return nil
}

// StreamResult holds the raw response body and a cleanup function returned
// by [DoStreamRequest].
type StreamResult struct {
	// Body is the raw HTTP response body. The caller reads from it lazily
	// (e.g. via a line scanner) to consume streaming events.
	Body io.ReadCloser
	// Cleanup MUST be called when the caller finishes reading Body (typically
	// via defer) to close the underlying HTTP connection.
	Cleanup func()
}

// DoStreamRequest sends a JSON HTTP request and returns the raw response body
// for lazy streaming consumption. Unlike [DoJSONRequest], it does NOT buffer
// the response or unmarshal it — the body is returned as-is so the caller can
// read incrementally (e.g. SSE events).
//
// Non-2xx responses are fully read, converted to [*APIError], and returned
// immediately (the body is closed in this case). On success, the caller MUST
// call result.Cleanup to close the body when done reading.
//
// No retry logic is applied — streaming responses cannot be safely retried
// because partial state may already have been consumed.
func DoStreamRequest(ctx context.Context, client *http.Client, cfg RequestConfig, reqBody any) (*StreamResult, error) {
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, cfg.Method, cfg.URL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range cfg.Headers {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBody))
		resp.Body.Close()
		apiErr := &APIError{
			StatusCode: resp.StatusCode,
			Body:       string(respBody),
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
		}
		if readErr != nil {
			return nil, fmt.Errorf("read error response: %w", errors.Join(readErr, apiErr))
		}
		return nil, apiErr
	}

	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			resp.Body.Close()
		case <-done:
		}
	}()

	return &StreamResult{
		Body: resp.Body,
		Cleanup: sync.OnceFunc(func() {
			resp.Body.Close()
			close(done)
		}),
	}, nil
}

func parseRetryAfter(value string) time.Duration {
	if seconds, err := strconv.ParseUint(value, 10, 63); err == nil {
		const maxDurationSeconds = uint64(1<<63-1) / uint64(time.Second)
		if seconds > 0 && seconds <= maxDurationSeconds {
			return time.Duration(seconds) * time.Second
		}
		return 0
	}

	retryAt, err := http.ParseTime(value)
	if err != nil {
		return 0
	}
	if delay := time.Until(retryAt); delay > 0 {
		return delay
	}
	return 0
}
