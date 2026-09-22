package codexauth

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dailz1/go-agent/agenttest"
	"github.com/dailz1/go-agent/llm/codex/auth"
)

func TestFileSourcePersistenceFailureCanBeRecorded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	state := storedState()
	state.Token.ExpiresAt = time.Now().Add(-time.Hour)
	if err := Save(t.Context(), path, state); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"access_token":"rotated","refresh_token":"rotated-refresh","expires_in":3600}`)
	}))
	defer server.Close()
	source, err := OpenFileSource(path, auth.RefreshConfig{TokenURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	_, refreshErr := source.Refresh(t.Context(), nil)
	var persistence *auth.Error
	if !errors.As(refreshErr, &persistence) || persistence.Stage != "persist" {
		t.Fatalf("refresh = %v", refreshErr)
	}
	recorder := agenttest.NewRecorder(agenttest.NewScriptedProvider(agenttest.Exchange{
		Method: agenttest.MethodChat, ChatErr: refreshErr,
	}))
	recorder.Chat(t.Context(), nil, nil)
	data, err := recorder.Bytes()
	if err != nil {
		t.Fatalf("default source produced an unrecordable error: %v", err)
	}
	replay, err := agenttest.NewReplayer(data)
	if err != nil {
		t.Fatal(err)
	}
	_, _, replayed := replay.Chat(t.Context(), nil, nil)
	if !errors.As(replayed, &persistence) || persistence.Stage != "persist" {
		t.Fatalf("replayed = %v", replayed)
	}
}

func TestFileSourceRotationSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	state := storedState()
	state.Token.ExpiresAt = time.Now().Add(-time.Hour)
	if err := Save(t.Context(), path, state); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		if err := r.ParseForm(); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		want := "refresh"
		if call == 2 {
			want = "rotated-refresh"
		}
		if r.Form.Get("refresh_token") != want {
			t.Errorf("refresh credential = %q, want %q", r.Form.Get("refresh_token"), want)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"new-access","refresh_token":"rotated-refresh","expires_in":3600}`)
	}))
	defer server.Close()
	cfg := auth.RefreshConfig{TokenURL: server.URL}
	source, err := OpenFileSource(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := source.Refresh(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenFileSource(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := reopened.Refresh(t.Context(), &got); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("token requests = %d", calls.Load())
	}
}

func TestFileSourceLockLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := Save(t.Context(), path, storedState()); err != nil {
		t.Fatal(err)
	}
	source, err := OpenFileSource(path, auth.RefreshConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := source.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := OpenFileSource(path, auth.RefreshConfig{}); !errors.Is(err, ErrBusy) {
		t.Fatalf("second source = %v", err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Refresh(t.Context(), nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed source = %v", err)
	}
	reopened, err := OpenFileSource(path, auth.RefreshConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestFileSourceLeavesForeignAndStaleLocks(t *testing.T) {
	for _, tc := range []struct {
		name    string
		replace bool
	}{
		{name: "stale"},
		{name: "replaced", replace: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "auth.json")
			if err := Save(t.Context(), path, storedState()); err != nil {
				t.Fatal(err)
			}
			var source *FileSource
			if tc.replace {
				var err error
				source, err = OpenFileSource(path, auth.RefreshConfig{})
				if err != nil {
					t.Fatal(err)
				}
			}
			data := []byte(`{"owner":"someone-else","pid":1}`)
			if err := os.WriteFile(path+".lock", data, 0o600); err != nil {
				t.Fatal(err)
			}
			if source != nil {
				if err := source.Close(); err == nil {
					t.Fatal("foreign lock removed")
				}
			} else if _, err := OpenFileSource(path, auth.RefreshConfig{}); !errors.Is(err, ErrBusy) {
				t.Fatalf("stale lock = %v", err)
			}
			after, err := os.ReadFile(path + ".lock")
			if err != nil || string(after) != string(data) {
				t.Fatalf("lock modified: %v", err)
			}
		})
	}
}
