package auth

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRefreshRejectedGenerationAndLate401(t *testing.T) {
	var posts int
	s := sourceFor(t, initialState(), RefreshConfig{ClientID: ClientID, TokenURL: Issuer + "/oauth/token",
		HTTPClient: &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
			posts++
			if posts == 1 {
				return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader(`{"error":"temporarily_unavailable"}`))}, nil
			}
			return successResponse(), nil
		})}, Persist: func(context.Context, State) error { return nil }})
	s.state.Token.ExpiresAt = testNow.Add(time.Hour)
	old, err := s.Token(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Refresh(t.Context(), &old); err == nil {
		t.Fatal("expected temporary failure")
	}
	got, err := s.Refresh(t.Context(), nil)
	if err != nil || got.AccessToken != "new" || posts != 2 {
		t.Fatal("rejected token was reused")
	}
	got, err = s.Refresh(t.Context(), &old)
	if err != nil || got.AccessToken != "new" || posts != 2 {
		t.Fatal("late 401 refreshed current token")
	}
}

func TestRefreshInvalidGrantAndAccountMismatch(t *testing.T) {
	for _, tc := range []struct {
		name, body, code string
		status           int
	}{
		{"invalid_grant", `{"error":"invalid_grant","error_description":"secret"}`, "invalid_grant", 400},
		{"revoked", `{"error":{"code":"refresh_token_reused"}}`, "refresh_token_reused", 400},
		{"account", `{"access_token":"` + jwt(`{"https://api.openai.com/auth":{"chatgpt_account_id":"other"}}`) + `"}`, "account_mismatch", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var posts int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				posts++
				w.WriteHeader(tc.status)
				if _, err := io.WriteString(w, tc.body); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			s := sourceFor(t, initialState(), RefreshConfig{ClientID: ClientID, TokenURL: server.URL,
				Persist: func(context.Context, State) error { t.Error("unexpected save"); return nil }})
			for range 2 {
				_, err := s.Refresh(t.Context(), nil)
				var ae *Error
				if !errors.As(err, &ae) || ae.Code != tc.code || !errors.Is(err, ErrLoginRequired) {
					t.Fatalf("error = %v", err)
				}
				if strings.Contains(err.Error(), "secret") {
					t.Fatal("OAuth body leaked")
				}
			}
			if posts != 1 {
				t.Fatal("terminal authorization retried")
			}
		})
	}
}

func TestRefreshUnknownPOSTOutcome(t *testing.T) {
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Error(err)
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		if err := conn.Close(); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	s := sourceFor(t, initialState(), RefreshConfig{ClientID: ClientID, TokenURL: server.URL,
		Persist: func(context.Context, State) error { t.Error("unexpected save"); return nil }})
	for range 2 {
		_, err := s.Refresh(t.Context(), nil)
		var ne net.Error
		if !errors.Is(err, ErrLoginRequired) || !errors.As(err, &ne) {
			t.Fatalf("error chain = %v", err)
		}
	}
	if posts.Load() != 1 {
		t.Fatal("old refresh token resent")
	}
}

func TestRefreshPreSendNetworkFailureAndCanceledContext(t *testing.T) {
	cause := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	var posts int
	s := sourceFor(t, initialState(), RefreshConfig{ClientID: ClientID, TokenURL: Issuer + "/oauth/token",
		HTTPClient: &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
			posts++
			return nil, cause
		})}, Persist: func(context.Context, State) error { return nil }})
	for range 2 {
		_, err := s.Refresh(t.Context(), nil)
		var ae *Error
		if !errors.As(err, &ae) || !ae.Temporary || errors.Is(err, ErrLoginRequired) || !errors.Is(err, cause) {
			t.Fatalf("error chain = %v", err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.Refresh(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := s.Token(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if posts != 2 {
		t.Fatal("canceled call made network request")
	}
}
