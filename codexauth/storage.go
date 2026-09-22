// Package codexauth provides explicit file storage and interactive PKCE login for
// the Codex subscription provider. It never writes the official CLI's auth file.
package codexauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/dailz1/go-agent/llm/codex/auth"
)

const storeLimit = 1 << 20

type credentialFile struct {
	Version     int         `json:"version"`
	AuthMode    string      `json:"auth_mode"`
	APIKey      string      `json:"OPENAI_API_KEY,omitempty"`
	Tokens      tokenFields `json:"tokens"`
	ExpiresAt   time.Time   `json:"expires_at"`
	LastRefresh time.Time   `json:"last_refresh"`
}

type tokenFields struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	IDToken      string `json:"id_token,omitempty"`
	AccountID    string `json:"account_id,omitempty"`
}

// DefaultPath explicitly resolves this application's independent store.
func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".go-agent", "codex", "auth.json"), nil
}

// Load reads an independent versioned store without refreshing credentials.
func Load(path string) (auth.State, error) {
	file, err := readCredentialFile(path)
	if err != nil {
		return auth.State{}, err
	}
	if file.Version != 1 || file.AuthMode != "chatgpt" || file.APIKey != "" {
		return auth.State{}, errors.New("unsupported codex credential store; API keys require openairesponses")
	}
	if file.Tokens.AccessToken == "" || file.Tokens.RefreshToken == "" {
		return auth.State{}, &auth.Error{Stage: "token", LoginRequired: true, Message: "missing subscription credentials"}
	}
	return auth.State{
		Token: auth.Token{
			AccessToken: file.Tokens.AccessToken,
			AccountID:   file.Tokens.AccountID,
			ExpiresAt:   file.ExpiresAt,
		},
		RefreshToken: file.Tokens.RefreshToken,
		IDToken:      file.Tokens.IDToken,
		LastRefresh:  file.LastRefresh,
	}, nil
}

// Save atomically replaces an independent store. Callers must own its file lock.
// A persistence failure must not be followed by use of the unpersisted token.
func Save(ctx context.Context, path string, state auth.State) error {
	if err := save(ctx, path, state); err != nil {
		return &auth.Error{Stage: "persist", Message: "save codex credentials", Cause: err}
	}
	return nil
}

func save(ctx context.Context, path string, state auth.State) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ownStorePath(path); err != nil {
		return err
	}
	if state.Token.AccessToken == "" || state.RefreshToken == "" {
		return errors.New("missing subscription credentials")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(credentialFile{
		Version: 1, AuthMode: "chatgpt",
		Tokens: tokenFields{
			AccessToken: state.Token.AccessToken, RefreshToken: state.RefreshToken,
			IDToken: state.IDToken, AccountID: state.Token.AccountID,
		},
		ExpiresAt: state.Token.ExpiresAt, LastRefresh: state.LastRefresh,
	})
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".auth-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	return syncDirectory(dir)
}

func readCredentialFile(path string) (credentialFile, error) {
	var result credentialFile
	file, err := os.Open(path)
	if err != nil {
		return result, fmt.Errorf("open codex credentials: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, storeLimit+1))
	if err != nil {
		return result, fmt.Errorf("read codex credentials: %w", err)
	}
	if len(data) > storeLimit {
		return result, errors.New("codex credentials exceed size limit")
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return result, errors.New("invalid codex credential JSON")
	}
	return result, nil
}

func ownStorePath(path string) error {
	if path == "" {
		return errors.New("empty codex credential path")
	}
	clean := filepath.Clean(path)
	resolved, err := filepath.EvalSymlinks(clean)
	if err == nil {
		clean = resolved
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("resolve credential path: %w", err)
	} else if parent, err := filepath.EvalSymlinks(filepath.Dir(clean)); err == nil {
		clean = filepath.Join(parent, filepath.Base(clean))
	}
	if filepath.Base(clean) == "auth.json" && filepath.Base(filepath.Dir(clean)) == ".codex" {
		return errors.New("official codex auth.json is read-only; use an independent store")
	}
	return nil
}
