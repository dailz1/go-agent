package auth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRefreshTemporaryFailureIsSharedBy32Waiters(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var posts atomic.Int32
	s := sourceFor(t, initialState(), RefreshConfig{
		ClientID: ClientID, TokenURL: Issuer + "/oauth/token",
		HTTPClient: &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
			if posts.Add(1) > 1 {
				return successResponse(), nil
			}
			close(started)
			select {
			case <-release:
				return &http.Response{StatusCode: 503,
					Body: io.NopCloser(strings.NewReader(`{"error":"temporarily_unavailable"}`))}, nil
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		})},
		Persist: func(context.Context, State) error { return nil },
	})
	results := make(chan error, 32)
	go func() { _, err := s.Refresh(t.Context(), nil); results <- err }()
	receive(t, started)
	entered := make(chan struct{}, 31)
	for range 31 {
		go func() {
			_, err := s.Refresh(&observedContext{Context: t.Context(), entered: entered}, nil)
			results <- err
		}()
	}
	for range 31 {
		receive(t, entered)
	}
	close(release)
	for range 32 {
		err := receive(t, results)
		var ae *Error
		if !errors.As(err, &ae) || !ae.Temporary || ae.Code != "temporarily_unavailable" {
			t.Fatalf("batch error = %v", err)
		}
	}
	if posts.Load() != 1 {
		t.Fatal("waiters retried a failed flight")
	}
	if _, err := s.Refresh(t.Context(), nil); err != nil || posts.Load() != 2 {
		t.Fatalf("new call could not retry temporary failure: %v", err)
	}
}
