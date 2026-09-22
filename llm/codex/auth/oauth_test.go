package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func jwt(claims string) string {
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".signature"
}

func TestPKCEAndAuthorizationProfile(t *testing.T) {
	first, err := NewPKCE()
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewPKCE()
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(first.Verifier))
	if first.State == second.State || first.Verifier == second.Verifier || len(first.Verifier) < 43 ||
		first.Challenge != base64.RawURLEncoding.EncodeToString(hash[:]) {
		t.Fatal("invalid PKCE generation")
	}
	u, err := url.Parse(AuthorizationURL(first))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	for key, want := range map[string]string{
		"client_id": ClientID, "redirect_uri": RedirectURI, "scope": Scopes,
		"response_type": "code", "state": first.State, "code_challenge": first.Challenge,
		"code_challenge_method": "S256",
	} {
		if q.Get(key) != want {
			t.Errorf("%s = %q, want %q", key, q.Get(key), want)
		}
	}
	if u.Scheme+"://"+u.Host != Issuer || u.Path != "/oauth/authorize" {
		t.Fatal("incorrect authorize endpoint")
	}
}

func TestExchangeAuthorizationCodeAndExpiresIn(t *testing.T) {
	access := jwt(`{"exp":1,"https://api.openai.com/auth":{"chatgpt_account_id":"account"}}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		for key, want := range map[string]string{"code": "code", "code_verifier": "verifier",
			"grant_type": "authorization_code", "client_id": "test-client", "redirect_uri": RedirectURI} {
			if r.Form.Get(key) != want {
				t.Errorf("%s = %q", key, r.Form.Get(key))
			}
		}
		if err := json.NewEncoder(w).Encode(map[string]any{
			"access_token": access, "refresh_token": "refresh", "expires_in": 3600,
		}); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	before := time.Now()
	state, err := Exchange(t.Context(), RefreshConfig{ClientID: "test-client", TokenURL: server.URL,
		HTTPClient: server.Client(), Persist: func(context.Context, State) error { t.Error("Exchange persisted"); return nil }},
		"code", "verifier")
	if err != nil {
		t.Fatal(err)
	}
	if state.Token.AccountID != "account" || state.RefreshToken != "refresh" ||
		state.LastRefresh.Before(before) || !state.Token.ExpiresAt.Equal(state.LastRefresh.Add(time.Hour)) {
		t.Fatalf("state = %#v", state)
	}
}

func TestParseTokenClaims(t *testing.T) {
	for _, tc := range []struct {
		name, claims, account string
		expiry                int64
		wantErr               bool
	}{
		{"namespaced", `{"exp":1234567890,"https://api.openai.com/auth":{"chatgpt_account_id":"a"}}`, "a", 1234567890, false},
		{"top_level", `{"chatgpt_account_id":"b"}`, "b", 0, false},
		{"missing", `{}`, "", 0, false},
		{"email_is_not_account", `{"email":"somebody@example.com","sub":"user-id"}`, "", 0, false},
		{"conflict", `{"chatgpt_account_id":"a","https://api.openai.com/auth":{"chatgpt_account_id":"b"}}`, "", 0, true},
		{"wrong_exp", `{"exp":"123"}`, "", 0, true},
		{"malformed", `{`, "", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token, err := ParseToken(jwt(tc.claims))
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v", err)
			}
			if tc.wantErr {
				return
			}
			if token.AccountID != tc.account || (tc.expiry == 0 && !token.ExpiresAt.IsZero()) ||
				(tc.expiry != 0 && token.ExpiresAt.Unix() != tc.expiry) {
				t.Fatalf("token = %#v", token)
			}
		})
	}
}
