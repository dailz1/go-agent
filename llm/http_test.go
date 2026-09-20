package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// testResponse is a simple struct for unmarshaling JSON responses in tests.
type testResponse struct {
	Message string `json:"message"`
	Count   int    `json:"count"`
}

func TestDoJSONRequest(t *testing.T) {
	t.Parallel()

	t.Run("200 response unmarshaled into respBody", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(testResponse{Message: "ok", Count: 42})
		}))
		defer srv.Close()

		var resp testResponse
		err := DoJSONRequest(context.Background(), srv.Client(), RequestConfig{
			Method: http.MethodPost,
			URL:    srv.URL,
		}, map[string]string{"prompt": "hello"}, &resp)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resp.Message != "ok" {
			t.Errorf("Message = %q, want %q", resp.Message, "ok")
		}
		if resp.Count != 42 {
			t.Errorf("Count = %d, want %d", resp.Count, 42)
		}
	})

	t.Run("non-2xx returns APIError", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			name           string
			statusCode     int
			body           string
			retryAfter     string
			wantRetryAfter time.Duration
		}{
			{"429 rate limit", 429, `{"error":"rate limited"}`, "2", 2 * time.Second},
			{"500 internal server error", 500, `{"error":"internal"}`, "", 0},
			{"403 forbidden", 403, `forbidden`, "invalid", 0},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Retry-After", tt.retryAfter)
					w.WriteHeader(tt.statusCode)
					w.Write([]byte(tt.body))
				}))
				defer srv.Close()

				var resp testResponse
				err := DoJSONRequest(context.Background(), srv.Client(), RequestConfig{
					Method: http.MethodPost,
					URL:    srv.URL,
				}, map[string]string{"q": "test"}, &resp)

				var apiErr *APIError
				if !errors.As(err, &apiErr) {
					t.Fatalf("expected *APIError, got %T: %v", err, err)
				}
				if apiErr.StatusCode != tt.statusCode {
					t.Errorf("StatusCode = %d, want %d", apiErr.StatusCode, tt.statusCode)
				}
				if apiErr.Body != tt.body {
					t.Errorf("Body = %q, want %q", apiErr.Body, tt.body)
				}
				if apiErr.RetryAfter != tt.wantRetryAfter {
					t.Errorf("RetryAfter = %v, want %v", apiErr.RetryAfter, tt.wantRetryAfter)
				}
			})
		}
	})

	t.Run("HTTP-date Retry-After is parsed", func(t *testing.T) {
		t.Parallel()
		retryAt := time.Now().Add(5 * time.Minute).UTC().Truncate(time.Second)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", retryAt.Format(http.TimeFormat))
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"unavailable"}`))
		}))
		defer srv.Close()

		beforeRequest := time.Until(retryAt)
		var resp testResponse
		err := DoJSONRequest(context.Background(), srv.Client(), RequestConfig{
			Method: http.MethodPost,
			URL:    srv.URL,
		}, map[string]string{}, &resp)
		afterRequest := time.Until(retryAt)

		var apiErr *APIError
		if !errors.As(err, &apiErr) {
			t.Fatalf("expected *APIError, got %T: %v", err, err)
		}
		if apiErr.RetryAfter < afterRequest || apiErr.RetryAfter > beforeRequest {
			t.Errorf("RetryAfter = %v, want between %v and %v", apiErr.RetryAfter, afterRequest, beforeRequest)
		}
	})

	t.Run("network error returns wrapped error", func(t *testing.T) {
		t.Parallel()
		// Use a server that's already closed to trigger a network error.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		srv.Close()

		var resp testResponse
		err := DoJSONRequest(context.Background(), srv.Client(), RequestConfig{
			Method: http.MethodPost,
			URL:    srv.URL,
		}, map[string]string{}, &resp)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "http request:") {
			t.Errorf("error should wrap http request failure, got: %v", err)
		}
		// Must NOT be an APIError — this is a transport-level failure.
		var apiErr *APIError
		if errors.As(err, &apiErr) {
			t.Error("network error should not be *APIError")
		}
	})

	t.Run("context cancellation", func(t *testing.T) {
		t.Parallel()
		handlerEntered := make(chan struct{})
		handlerRelease := make(chan struct{})
		handlerDone := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer close(handlerDone)
			close(handlerEntered)
			<-handlerRelease
		}))
		defer srv.Close()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		errs := make(chan error, 1)
		go func() {
			var resp testResponse
			errs <- DoJSONRequest(ctx, srv.Client(), RequestConfig{
				Method: http.MethodPost,
				URL:    srv.URL,
			}, map[string]string{}, &resp)
		}()

		select {
		case <-handlerEntered:
		case <-time.After(5 * time.Second):
			cancel()
			close(handlerRelease)
			select {
			case <-handlerDone:
				t.Fatalf("request handler never entered")
			case <-time.After(5 * time.Second):
				t.Fatalf("request handler never entered or exited after release")
			}
		}
		cancel()

		err := <-errs
		close(handlerRelease)
		<-handlerDone
		if err == nil {
			t.Fatal("expected error from cancelled context, got nil")
		}
		if !strings.Contains(err.Error(), "http request:") {
			t.Errorf("error should wrap http request failure, got: %v", err)
		}
	})

	t.Run("custom headers are set on request", func(t *testing.T) {
		t.Parallel()
		var gotHeaders http.Header
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotHeaders = r.Header.Clone()
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{}`))
		}))
		defer srv.Close()

		var resp testResponse
		err := DoJSONRequest(context.Background(), srv.Client(), RequestConfig{
			Method: http.MethodPost,
			URL:    srv.URL,
			Headers: map[string]string{
				"Authorization": "Bearer test-token-123",
				"X-Custom":      "custom-value",
			},
		}, map[string]string{}, &resp)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if gotHeaders.Get("Authorization") != "Bearer test-token-123" {
			t.Errorf("Authorization = %q, want %q", gotHeaders.Get("Authorization"), "Bearer test-token-123")
		}
		if gotHeaders.Get("X-Custom") != "custom-value" {
			t.Errorf("X-Custom = %q, want %q", gotHeaders.Get("X-Custom"), "custom-value")
		}
	})

	t.Run("Content-Type application/json is set automatically", func(t *testing.T) {
		t.Parallel()
		var gotContentType string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotContentType = r.Header.Get("Content-Type")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{}`))
		}))
		defer srv.Close()

		var resp testResponse
		err := DoJSONRequest(context.Background(), srv.Client(), RequestConfig{
			Method: http.MethodPost,
			URL:    srv.URL,
		}, map[string]string{}, &resp)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if gotContentType != "application/json" {
			t.Errorf("Content-Type = %q, want %q", gotContentType, "application/json")
		}
	})

	t.Run("marshal error returns wrapped error", func(t *testing.T) {
		t.Parallel()
		var resp testResponse
		err := DoJSONRequest(context.Background(), http.DefaultClient, RequestConfig{
			Method: http.MethodPost,
			URL:    "http://unused.example.com",
		}, unmarshalable{}, &resp)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "marshal request:") {
			t.Errorf("error should contain 'marshal request:', got: %v", err)
		}
	})

	t.Run("unmarshal error for invalid JSON response", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`not valid json`))
		}))
		defer srv.Close()

		var resp testResponse
		err := DoJSONRequest(context.Background(), srv.Client(), RequestConfig{
			Method: http.MethodPost,
			URL:    srv.URL,
		}, map[string]string{}, &resp)
		if err == nil {
			t.Fatal("expected unmarshal error, got nil")
		}
		if !strings.Contains(err.Error(), "unmarshal response:") {
			t.Errorf("error should contain 'unmarshal response:', got: %v", err)
		}
	})

	t.Run("request body is correctly serialized", func(t *testing.T) {
		t.Parallel()
		var gotBody []byte
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotBody, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{}`))
		}))
		defer srv.Close()

		reqBody := map[string]string{"prompt": "hello", "model": "gpt-4"}
		var resp testResponse
		err := DoJSONRequest(context.Background(), srv.Client(), RequestConfig{
			Method: http.MethodPost,
			URL:    srv.URL,
		}, reqBody, &resp)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var decoded map[string]string
		if err := json.Unmarshal(gotBody, &decoded); err != nil {
			t.Fatalf("failed to decode request body: %v", err)
		}
		if decoded["prompt"] != "hello" {
			t.Errorf("prompt = %q, want %q", decoded["prompt"], "hello")
		}
		if decoded["model"] != "gpt-4" {
			t.Errorf("model = %q, want %q", decoded["model"], "gpt-4")
		}
	})
}

func TestDoStreamRequest(t *testing.T) {
	t.Parallel()

	t.Run("200 response returns StreamResult with readable Body", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("data: chunk1\n\ndata: chunk2\n\n"))
		}))
		defer srv.Close()

		result, err := DoStreamRequest(context.Background(), srv.Client(), RequestConfig{
			Method: http.MethodPost,
			URL:    srv.URL,
		}, map[string]string{"prompt": "stream me"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result == nil {
			t.Fatal("expected non-nil result")
		}
		if result.Body == nil {
			t.Fatal("expected non-nil Body")
		}
		if result.Cleanup == nil {
			t.Fatal("expected non-nil Cleanup")
		}

		body, err := io.ReadAll(result.Body)
		if err != nil {
			t.Fatalf("failed to read body: %v", err)
		}
		expected := "data: chunk1\n\ndata: chunk2\n\n"
		if string(body) != expected {
			t.Errorf("Body = %q, want %q", string(body), expected)
		}
		result.Cleanup()
	})

	t.Run("non-2xx returns APIError and closes body", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			name           string
			statusCode     int
			body           string
			retryAfter     string
			wantRetryAfter time.Duration
		}{
			{"429 rate limit", 429, `{"error":"slow down"}`, "2", 2 * time.Second},
			{"500 server error", 500, `internal server error`, "", 0},
			{"401 unauthorized", 401, `unauthorized`, "invalid", 0},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Retry-After", tt.retryAfter)
					w.WriteHeader(tt.statusCode)
					w.Write([]byte(tt.body))
				}))
				defer srv.Close()

				result, err := DoStreamRequest(context.Background(), srv.Client(), RequestConfig{
					Method: http.MethodPost,
					URL:    srv.URL,
				}, map[string]string{})
				if result != nil {
					t.Error("expected nil result on non-2xx")
				}
				var apiErr *APIError
				if !errors.As(err, &apiErr) {
					t.Fatalf("expected *APIError, got %T: %v", err, err)
				}
				if apiErr.StatusCode != tt.statusCode {
					t.Errorf("StatusCode = %d, want %d", apiErr.StatusCode, tt.statusCode)
				}
				if apiErr.Body != tt.body {
					t.Errorf("Body = %q, want %q", apiErr.Body, tt.body)
				}
				if apiErr.RetryAfter != tt.wantRetryAfter {
					t.Errorf("RetryAfter = %v, want %v", apiErr.RetryAfter, tt.wantRetryAfter)
				}
			})
		}
	})

	t.Run("non-2xx body cancellation preserves API and context errors", func(t *testing.T) {
		t.Parallel()

		bodyReadStarted := make(chan struct{}, 1)
		handlerDone := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer close(handlerDone)
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"partial`))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}))
		defer srv.Close()

		client := &http.Client{Transport: &readSignalTransport{
			base:    srv.Client().Transport,
			started: bodyReadStarted,
		}}
		ctx, cancel := context.WithCancel(context.Background())
		requestDone := make(chan error, 1)
		go func() {
			_, err := DoStreamRequest(ctx, client, RequestConfig{
				Method: http.MethodPost,
				URL:    srv.URL,
			}, map[string]string{})
			requestDone <- err
		}()

		select {
		case <-bodyReadStarted:
			cancel()
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for the response body read to start")
		}

		var err error
		select {
		case err = <-requestDone:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for DoStreamRequest to return")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("error = %v, want context.Canceled in error chain", err)
		}
		var apiErr *APIError
		if !errors.As(err, &apiErr) {
			t.Fatalf("error = %T %v, want *APIError in error chain", err, err)
		}
		if apiErr.StatusCode != http.StatusTooManyRequests {
			t.Errorf("StatusCode = %d, want %d", apiErr.StatusCode, http.StatusTooManyRequests)
		}

		select {
		case <-handlerDone:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for handler shutdown")
		}
	})

	t.Run("network error returns wrapped error", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		srv.Close()

		result, err := DoStreamRequest(context.Background(), srv.Client(), RequestConfig{
			Method: http.MethodPost,
			URL:    srv.URL,
		}, map[string]string{})
		if result != nil {
			t.Error("expected nil result on network error")
		}
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "http request:") {
			t.Errorf("error should wrap http request failure, got: %v", err)
		}
		var apiErr *APIError
		if errors.As(err, &apiErr) {
			t.Error("network error should not be *APIError")
		}
	})

	t.Run("Cleanup closes connection", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("some data"))
		}))
		defer srv.Close()

		result, err := DoStreamRequest(context.Background(), srv.Client(), RequestConfig{
			Method: http.MethodPost,
			URL:    srv.URL,
		}, map[string]string{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		result.Cleanup()

		// After cleanup, reading from body should fail.
		_, readErr := io.ReadAll(result.Body)
		if readErr == nil {
			t.Error("expected error reading from body after Cleanup, got nil")
		}
	})

	t.Run("context cancellation", func(t *testing.T) {
		t.Parallel()
		handlerEntered := make(chan struct{})
		handlerRelease := make(chan struct{})
		handlerDone := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer close(handlerDone)
			close(handlerEntered)
			<-handlerRelease
		}))
		defer srv.Close()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		type response struct {
			result *StreamResult
			err    error
		}
		responses := make(chan response, 1)
		go func() {
			result, err := DoStreamRequest(ctx, srv.Client(), RequestConfig{
				Method: http.MethodPost,
				URL:    srv.URL,
			}, map[string]string{})
			responses <- response{result: result, err: err}
		}()

		select {
		case <-handlerEntered:
		case <-time.After(5 * time.Second):
			cancel()
			close(handlerRelease)
			select {
			case <-handlerDone:
				t.Fatalf("stream request handler never entered")
			case <-time.After(5 * time.Second):
				t.Fatalf("stream request handler never entered or exited after release")
			}
		}
		cancel()

		outcome := <-responses
		result, err := outcome.result, outcome.err
		close(handlerRelease)
		<-handlerDone
		if result != nil {
			t.Error("expected nil result on cancelled context")
		}
		if err == nil {
			t.Fatal("expected error from cancelled context, got nil")
		}
		if !strings.Contains(err.Error(), "http request:") {
			t.Errorf("error should wrap http request failure, got: %v", err)
		}
	})

	t.Run("marshal error returns wrapped error", func(t *testing.T) {
		t.Parallel()
		result, err := DoStreamRequest(context.Background(), http.DefaultClient, RequestConfig{
			Method: http.MethodPost,
			URL:    "http://unused.example.com",
		}, unmarshalable{})
		if result != nil {
			t.Error("expected nil result on marshal error")
		}
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "marshal request:") {
			t.Errorf("error should contain 'marshal request:', got: %v", err)
		}
	})

	t.Run("custom headers and Content-Type are set", func(t *testing.T) {
		t.Parallel()
		var gotHeaders http.Header
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotHeaders = r.Header.Clone()
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("ok"))
		}))
		defer srv.Close()

		result, err := DoStreamRequest(context.Background(), srv.Client(), RequestConfig{
			Method: http.MethodPost,
			URL:    srv.URL,
			Headers: map[string]string{
				"Authorization": "Bearer stream-token",
			},
		}, map[string]string{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		defer result.Cleanup()

		if gotHeaders.Get("Content-Type") != "application/json" {
			t.Errorf("Content-Type = %q, want %q", gotHeaders.Get("Content-Type"), "application/json")
		}
		if gotHeaders.Get("Authorization") != "Bearer stream-token" {
			t.Errorf("Authorization = %q, want %q", gotHeaders.Get("Authorization"), "Bearer stream-token")
		}
	})
}

// unmarshalable is a type that causes json.Marshal to fail.
type unmarshalable struct {
	Ch chan struct{}
}

type readSignalTransport struct {
	base    http.RoundTripper
	started chan<- struct{}
}

func (t *readSignalTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err == nil {
		resp.Body = &readSignalBody{ReadCloser: resp.Body, started: t.started}
	}
	return resp, err
}

type readSignalBody struct {
	io.ReadCloser
	started chan<- struct{}
}

func (b *readSignalBody) Read(p []byte) (int, error) {
	select {
	case b.started <- struct{}{}:
	default:
	}
	return b.ReadCloser.Read(p)
}
