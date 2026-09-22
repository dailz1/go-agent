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

func TestRefreshTokenResponseVariants(t *testing.T) {
	for _, tc := range []struct {
		name, body, account, refresh string
		expiry                       time.Time
		wantErr                      bool
	}{
		{"jwt_expiry", `{"access_token":"` + jwt(`{"exp":1800000000}`) + `"}`, "account", "refresh-old", time.Unix(1800000000, 0), false},
		{"missing_expiry", `{"access_token":"opaque"}`, "account", "refresh-old", time.Time{}, false},
		{"zero_expiry_precedence", `{"access_token":"` + jwt(`{"exp":1800000000}`) + `","expires_in":0}`, "account", "refresh-old", testNow, false},
		{"id_account", `{"access_token":"opaque","id_token":"` + jwt(`{"https://api.openai.com/auth":{"chatgpt_account_id":"account"}}`) + `"}`,
			"account", "refresh-old", time.Time{}, false},
		{"id_conflict", `{"access_token":"opaque","id_token":"` + jwt(`{"chatgpt_account_id":"other"}`) + `"}`, "", "", time.Time{}, true},
		{"jwt_conflict", `{"access_token":"` + jwt(`{"chatgpt_account_id":"account"}`) + `","id_token":"` + jwt(`{"chatgpt_account_id":"other"}`) + `"}`,
			"", "", time.Time{}, true},
		{"negative_expiry", `{"access_token":"opaque","expires_in":-1}`, "", "", time.Time{}, true},
		{"overflow_expiry", `{"access_token":"opaque","expires_in":9223372036854775807}`, "", "", time.Time{}, true},
		{"missing_access", `{"refresh_token":"rotated"}`, "", "", time.Time{}, true},
		{"malformed", `{`, "", "", time.Time{}, true},
		{"huge", strings.Repeat("x", maxTokenResponse+1), "", "", time.Time{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var saves int
			var saved State
			s := sourceFor(t, initialState(), RefreshConfig{ClientID: ClientID, TokenURL: Issuer + "/oauth/token",
				HTTPClient: &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
				})}, Persist: func(_ context.Context, state State) error { saves++; saved = state; return nil }})
			_, err := s.Refresh(t.Context(), nil)
			if tc.wantErr {
				if !errors.Is(err, ErrLoginRequired) || saves != 0 {
					t.Fatalf("error=%v saves=%d", err, saves)
				}
				return
			}
			if err != nil || saved.Token.AccountID != tc.account || saved.RefreshToken != tc.refresh ||
				!saved.Token.ExpiresAt.Equal(tc.expiry) {
				t.Fatalf("state=%#v error=%v", saved, err)
			}
		})
	}
}

func TestOAuthRedirectDoesNotLeakOrMutateClient(t *testing.T) {
	var destinationCalls atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		destinationCalls.Add(1)
	}))
	defer destination.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	var redirects int
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { redirects++; return nil }}
	_, err := Exchange(t.Context(), RefreshConfig{TokenURL: server.URL, HTTPClient: client}, "code", "verifier")
	if err == nil || destinationCalls.Load() != 0 || redirects != 0 {
		t.Fatal("OAuth redirect followed")
	}
	if err := client.CheckRedirect(nil, nil); err != nil || redirects != 1 {
		t.Fatal("caller client changed")
	}
}

func TestNewRefreshingSourceRejectsIncompleteConfiguration(t *testing.T) {
	valid := RefreshConfig{ClientID: ClientID, TokenURL: Issuer + "/oauth/token",
		Persist: func(context.Context, State) error { return nil }}
	for _, tc := range []struct {
		name string
		edit func(*State, *RefreshConfig)
	}{
		{"client", func(_ *State, c *RefreshConfig) { c.ClientID = "" }},
		{"url", func(_ *State, c *RefreshConfig) { c.TokenURL = "" }},
		{"remote_http", func(_ *State, c *RefreshConfig) { c.TokenURL = "http://example.com/token" }},
		{"userinfo", func(_ *State, c *RefreshConfig) { c.TokenURL = "https://user@example.com/token" }},
		{"query", func(_ *State, c *RefreshConfig) { c.TokenURL += "?secret=1" }},
		{"persist", func(_ *State, c *RefreshConfig) { c.Persist = nil }},
		{"refresh", func(s *State, _ *RefreshConfig) { s.RefreshToken = "" }},
		{"account", func(s *State, _ *RefreshConfig) { s.Token.AccountID = "" }},
		{"access", func(s *State, _ *RefreshConfig) { s.Token.AccessToken = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, cfg := initialState(), valid
			tc.edit(&state, &cfg)
			if _, err := NewRefreshingSource(state, cfg); err == nil {
				t.Fatal("accepted incomplete configuration")
			}
		})
	}
}

func TestRefreshCanceledBeforeConnectionIsNotUnknownOutcome(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	started := make(chan struct{})
	release := make(chan struct{})
	dialDone := make(chan struct{})
	defer func() { close(release); receive(t, dialDone) }()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		defer close(dialDone)
		close(started)
		<-release
		return nil, context.Canceled
	}}
	defer transport.CloseIdleConnections()
	s := sourceFor(t, initialState(), RefreshConfig{ClientID: ClientID, TokenURL: Issuer + "/oauth/token",
		HTTPClient: &http.Client{Transport: transport}, Persist: func(context.Context, State) error { return nil }})
	result := make(chan error, 1)
	go func() { _, err := s.Refresh(ctx, nil); result <- err }()
	receive(t, started)
	cancel()
	err := receive(t, result)
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrLoginRequired) {
		t.Fatalf("unsent request classified as unknown outcome: %v", err)
	}
}
