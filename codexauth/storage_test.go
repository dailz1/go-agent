package codexauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/dailz1/go-agent/llm/codex/auth"
)

func storedState() auth.State {
	return auth.State{
		Token: auth.Token{
			AccessToken: "access",
			AccountID:   "account",
			ExpiresAt:   time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC),
		},
		RefreshToken: "refresh",
		IDToken:      "id",
		LastRefresh:  time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC),
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store", "auth.json")
	want := storedState()
	if err := Save(t.Context(), path, want); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("state = %#v, want %#v", got, want)
	}
	for _, item := range []struct {
		name string
		path string
		mode os.FileMode
	}{
		{name: "directory", path: filepath.Dir(path), mode: 0o700},
		{name: "file", path: path, mode: 0o600},
	} {
		t.Run(item.name, func(t *testing.T) {
			info, err := os.Stat(item.path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != item.mode {
				t.Fatalf("permissions = %o, want %o", info.Mode().Perm(), item.mode)
			}
		})
	}
}

func TestSaveCanceledPreservesOriginal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	want := storedState()
	if err := Save(t.Context(), path, want); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	next := want
	next.RefreshToken = "rotated"
	if err := Save(ctx, path, next); !errors.Is(err, context.Canceled) {
		t.Fatalf("save = %v", err)
	}
	got, err := Load(path)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("original changed: %#v, %v", got, err)
	}
}

func TestSaveRejectsOfficialStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".codex", "auth.json")
	if err := Save(t.Context(), path, storedState()); err == nil {
		t.Fatal("official store was writable")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("official store created: %v", err)
	}
}

func TestSaveRenameFailureCleansTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "auth.json")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Save(t.Context(), target, storedState()); err == nil {
		t.Fatal("save over directory succeeded")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "auth.json" || !entries[0].IsDir() {
		t.Fatalf("original directory changed or temporary file leaked: %v", entries)
	}
}

func TestLoadRejectsInvalidStore(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
	}{
		{name: "version", data: `{"version":9,"auth_mode":"chatgpt","tokens":{"access_token":"a","refresh_token":"r"}}`},
		{name: "api key", data: `{"version":1,"auth_mode":"apikey","OPENAI_API_KEY":"secret"}`},
		{name: "missing refresh", data: `{"version":1,"auth_mode":"chatgpt","tokens":{"access_token":"a"}}`},
		{name: "trailing object", data: `{} {}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "auth.json")
			if err := os.WriteFile(path, []byte(tc.data), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatal("invalid store accepted")
			}
		})
	}
}

func TestOfficialAccessOnlyNeverRefreshesOrWrites(t *testing.T) {
	payload, err := json.Marshal(map[string]any{
		"exp":                         time.Now().Add(time.Hour).Unix(),
		"https://api.openai.com/auth": map[string]string{"chatgpt_account_id": "account"},
	})
	if err != nil {
		t.Fatal(err)
	}
	token := "e30." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
	data, err := json.Marshal(map[string]any{
		"auth_mode": "chatgpt",
		"tokens": map[string]string{
			"access_token": token, "refresh_token": "never-use", "account_id": "account",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := OpenOfficialSource(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := source.Refresh(t.Context(), nil)
	if err != nil || got.AccessToken != token {
		t.Fatalf("refresh = %#v, %v", got, err)
	}
	if _, err := source.Refresh(t.Context(), &got); !errors.Is(err, auth.ErrLoginRequired) {
		t.Fatalf("rejected credential = %v", err)
	}
	if _, err := source.Refresh(t.Context(), nil); !errors.Is(err, auth.ErrLoginRequired) {
		t.Fatalf("rejected token reused = %v", err)
	}
	snapshot, err := source.Token(t.Context())
	if err != nil || snapshot != got {
		t.Fatalf("snapshot = %#v, %v", snapshot, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(data) {
		t.Fatalf("official file modified: %v", err)
	}
}

func TestOfficialSourceRejectsUnusableCredentials(t *testing.T) {
	for _, tc := range []struct {
		name    string
		expiry  int64
		account string
	}{
		{name: "unknown expiry", account: "account"},
		{name: "near expiry", expiry: time.Now().Add(30 * time.Second).Unix(), account: "account"},
		{name: "expired", expiry: 1, account: "account"},
		{name: "missing account", expiry: time.Now().Add(time.Hour).Unix()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload, err := json.Marshal(map[string]any{"exp": tc.expiry})
			if err != nil {
				t.Fatal(err)
			}
			file := credentialFile{
				AuthMode: "chatgpt",
				Tokens: tokenFields{
					AccessToken: "e30." + base64.RawURLEncoding.EncodeToString(payload) + ".sig",
					AccountID:   tc.account,
				},
			}
			data, err := json.Marshal(file)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "auth.json")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			source, err := OpenOfficialSource(path)
			if err == nil {
				_, err = source.Refresh(t.Context(), nil)
			}
			if !errors.Is(err, auth.ErrLoginRequired) {
				t.Fatalf("unusable credentials accepted: %v", err)
			}
		})
	}
}
