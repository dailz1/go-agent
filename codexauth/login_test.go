package codexauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dailz1/go-agent/llm/codex/auth"
)

type routeTokenTransport struct {
	url       string
	transport http.RoundTripper
}

func (r routeTokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	copy := req.Clone(req.Context())
	target, err := url.Parse(r.url)
	if err != nil {
		return nil, err
	}
	copy.URL = target
	return r.transport.RoundTrip(copy)
}

func callback(t *testing.T, query url.Values, want int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, auth.RedirectURI+"?"+query.Encode(), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		t.Fatalf("callback status = %d, want %d", resp.StatusCode, want)
	}
}

func TestLoginPKCECallbackAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials", "auth.json")
	var calls atomic.Int32
	var challenge string
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if err := r.ParseForm(); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		valid := r.Form.Get("code") == "one-time-code" &&
			r.Form.Get("client_id") == auth.ClientID &&
			r.Form.Get("redirect_uri") == auth.RedirectURI &&
			r.Form.Get("grant_type") == "authorization_code" &&
			base64.RawURLEncoding.EncodeToString(sum[:]) == challenge
		if !valid {
			t.Error("token exchange did not match authorized PKCE request")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		payload := base64.RawURLEncoding.EncodeToString([]byte(
			`{"https://api.openai.com/auth":{"chatgpt_account_id":"account"}}`,
		))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":%q,"refresh_token":"new-refresh","expires_in":3600}`, "e30."+payload+".sig")
	}))
	defer tokenServer.Close()
	var output strings.Builder
	_, err := Login(t.Context(), LoginConfig{
		Path: path, Output: &output, Headless: true,
		HTTPClient: &http.Client{Transport: routeTokenTransport{
			url: tokenServer.URL, transport: http.DefaultTransport,
		}},
		OnAuthorize: func(ctx context.Context, address string) error {
			u, err := url.Parse(address)
			if err != nil {
				return err
			}
			q := u.Query()
			if q.Get("scope") != auth.Scopes || q.Get("redirect_uri") != auth.RedirectURI {
				t.Error("authorization profile mismatch")
			}
			challenge = q.Get("code_challenge")
			if _, err := OpenFileSource(path, auth.RefreshConfig{}); !errors.Is(err, ErrBusy) {
				t.Errorf("login did not own store: %v", err)
			}
			callback(t, url.Values{"state": {"wrong"}, "code": {"one-time-code"}}, http.StatusBadRequest)
			callback(t, url.Values{"state": {q.Get("state")}, "code": {"one-time-code"}}, http.StatusOK)
			callback(t, url.Values{"state": {q.Get("state")}, "code": {"one-time-code"}}, http.StatusConflict)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("token calls = %d", calls.Load())
	}
	source, err := OpenFileSource(path, auth.RefreshConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	token, err := source.Refresh(t.Context(), nil)
	if err != nil || token.AccountID != "account" {
		t.Fatalf("restarted source = %#v, %v", token, err)
	}
}

func TestLoginCanceledClosesListenerAndReleasesLock(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	path := filepath.Join(t.TempDir(), "auth.json")
	_, err := Login(ctx, LoginConfig{
		Path: path, Output: io.Discard,
		OnAuthorize: func(context.Context, string) error {
			cancel()
			return errors.New("browser unavailable")
		},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("login = %v", err)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:1455")
	if err != nil {
		t.Fatalf("listener leaked: %v", err)
	}
	listener.Close()
	lock, err := acquireLock(path)
	if err != nil {
		t.Fatalf("lock leaked: %v", err)
	}
	if err := lock.close(); err != nil {
		t.Fatal(err)
	}
}

func TestLoginPortOccupied(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:1455")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	called := false
	_, err = Login(t.Context(), LoginConfig{
		Path: filepath.Join(t.TempDir(), "auth.json"), Output: io.Discard,
		OnAuthorize: func(context.Context, string) error {
			called = true
			return nil
		},
	})
	if err == nil || called {
		t.Fatalf("occupied port: err=%v authorize=%v", err, called)
	}
}

func TestLoginCallbackErrorIsRedacted(t *testing.T) {
	_, err := Login(t.Context(), LoginConfig{
		Path: filepath.Join(t.TempDir(), "auth.json"), Output: io.Discard,
		OnAuthorize: func(ctx context.Context, address string) error {
			u, err := url.Parse(address)
			if err != nil {
				return err
			}
			callback(t, url.Values{
				"state":             {u.Query().Get("state")},
				"error":             {"denied-secret-value"},
				"error_description": {"private-callback-data"},
			}, http.StatusOK)
			return nil
		},
	})
	var authErr *auth.Error
	if !errors.As(err, &authErr) {
		t.Fatalf("error type = %T: %v", err, err)
	}
	data, _ := json.Marshal(authErr)
	if strings.Contains(string(data), "secret-value") || strings.Contains(err.Error(), "private-callback-data") {
		t.Fatalf("callback leaked: %v", err)
	}
}
