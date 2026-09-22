package auth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestExchangeIDAccountFallbackAndFailures(t *testing.T) {
	for _, tc := range []struct {
		name, body, code string
	}{
		{"id_fallback", `{"access_token":"opaque","refresh_token":"r","id_token":"` +
			jwt(`{"https://api.openai.com/auth":{"chatgpt_account_id":"account"}}`) + `"}`, ""},
		{"account_missing", `{"access_token":"opaque","refresh_token":"r"}`, "account_missing"},
		{"refresh_missing", `{"access_token":"opaque"}`, "credentials"},
		{"invalid_id", `{"access_token":"opaque","refresh_token":"r","id_token":"broken"}`, "invalid_token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, err := Exchange(t.Context(), RefreshConfig{
				HTTPClient: &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
					if r.URL.String() != Issuer+"/oauth/token" {
						t.Error("default endpoint changed")
					}
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
				})},
			}, "code", "verifier")
			if tc.code == "" {
				if err != nil || state.Token.AccountID != "account" {
					t.Fatalf("account fallback: %v, %#v", err, state)
				}
				return
			}
			var ae *Error
			if !errors.As(err, &ae) || ae.Stage != "token" || ae.Code != tc.code {
				t.Fatalf("exchange error = %v", err)
			}
		})
	}
}

func TestExchangeCanceledContextChain(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := Exchange(ctx, RefreshConfig{
		HTTPClient: &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
			t.Error("canceled exchange performed I/O")
			return nil, errors.New("unexpected I/O")
		})},
	}, "code", "verifier")
	var ae *Error
	if !errors.Is(err, context.Canceled) || !errors.As(err, &ae) || ae.Stage != "token" {
		t.Fatalf("error chain = %v", err)
	}
}

type interruptedBody struct{}

func (interruptedBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (interruptedBody) Close() error             { return nil }

func TestRefreshInterruptedResponseRequiresLogin(t *testing.T) {
	var calls int
	s := sourceFor(t, initialState(), RefreshConfig{
		ClientID: ClientID, TokenURL: Issuer + "/oauth/token",
		HTTPClient: &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: 200, Body: interruptedBody{}}, nil
		})},
		Persist: func(context.Context, State) error { t.Error("incomplete response persisted"); return nil },
	})
	for range 2 {
		_, err := s.Refresh(t.Context(), nil)
		if !errors.Is(err, io.ErrUnexpectedEOF) || !errors.Is(err, ErrLoginRequired) {
			t.Fatalf("error chain = %v", err)
		}
	}
	if calls != 1 {
		t.Fatal("unknown outcome retried")
	}
}

func TestParseTokenRejectsNonObjectClaims(t *testing.T) {
	for _, input := range []string{jwt(`null`), jwt(`[]`), "opaque", "a.???.c"} {
		if _, err := ParseToken(input); err == nil {
			t.Fatalf("accepted invalid token %q", input)
		}
	}
}
