package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

var testNow = time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)

func initialState() State {
	return State{Token: Token{AccessToken: "old", AccountID: "account", ExpiresAt: testNow},
		RefreshToken: "refresh-old", IDToken: "id-old"}
}

func sourceFor(t *testing.T, state State, cfg RefreshConfig) *RefreshingSource {
	t.Helper()
	s, err := NewRefreshingSource(state, cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return testNow }
	return s
}

func TestRefreshThresholdAndSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name      string
		expiry    time.Time
		wantCalls int32
	}{
		{"valid", testNow.Add(61 * time.Second), 0},
		{"threshold", testNow.Add(60 * time.Second), 1},
		{"expired", testNow.Add(-time.Second), 1},
		{"unknown", time.Time{}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if err := r.ParseForm(); err != nil {
					t.Error(err)
				}
				if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "refresh-old" {
					t.Error("incorrect refresh form")
				}
				if err := json.NewEncoder(w).Encode(map[string]any{
					"access_token": "new", "refresh_token": "refresh-new", "expires_in": 3600,
				}); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			state := initialState()
			state.Token.ExpiresAt = tc.expiry
			var saved State
			s := sourceFor(t, state, RefreshConfig{
				ClientID: ClientID, TokenURL: server.URL, HTTPClient: server.Client(),
				Persist: func(_ context.Context, next State) error { saved = next; return nil },
			})
			for range 3 {
				got, err := s.Token(t.Context())
				if err != nil || got != state.Token || calls.Load() != 0 {
					t.Fatal("snapshot caused I/O")
				}
			}
			got, err := s.Refresh(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if calls.Load() != tc.wantCalls {
				t.Fatalf("calls = %d", calls.Load())
			}
			if tc.wantCalls == 1 && (got.AccessToken != "new" || saved.RefreshToken != "refresh-new" ||
				saved.IDToken != "id-old" || !saved.Token.ExpiresAt.Equal(testNow.Add(time.Hour))) {
				t.Fatalf("incorrect rotation: %#v", saved)
			}
		})
	}
}

func TestRefreshPersistenceRetryOnly(t *testing.T) {
	var posts, saves int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		if _, err := w.Write([]byte(`{"access_token":"new","refresh_token":"rotated","expires_in":3600}`)); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	cause := errors.New("storage unavailable")
	s := sourceFor(t, initialState(), RefreshConfig{
		ClientID: ClientID, TokenURL: server.URL,
		Persist: func(_ context.Context, state State) error {
			saves++
			if state.RefreshToken != "rotated" {
				t.Error("rotation lost")
			}
			if saves == 1 {
				return cause
			}
			return nil
		},
	})
	_, err := s.Refresh(t.Context(), nil)
	var ae *Error
	if !errors.As(err, &ae) || ae.Stage != "persist" || !errors.Is(err, cause) {
		t.Fatalf("error = %v", err)
	}
	old, err := s.Token(t.Context())
	if err != nil || old.AccessToken != "old" || saves != 1 {
		t.Fatal("published unpersisted token")
	}
	got, err := s.Refresh(t.Context(), nil)
	if err != nil || got.AccessToken != "new" || posts != 1 || saves != 2 {
		t.Fatalf("retry: %v, posts=%d saves=%d", err, posts, saves)
	}
}
