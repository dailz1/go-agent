package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dailz1/go-agent/codexauth"
	"github.com/dailz1/go-agent/llm/codex/auth"
)

func TestRunStatusRedactsCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	state := auth.State{
		Token: auth.Token{
			AccessToken: "access-secret", AccountID: "account-private",
			ExpiresAt: time.Now().Add(time.Hour),
		},
		RefreshToken: "refresh-secret",
	}
	if err := codexauth.Save(t.Context(), path, state); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	if err := run(t.Context(), []string{"status", "--store", path}, &out, &stderr); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Account    string `json:"account"`
		NeedsLogin bool   `json:"needs_login"`
		ExpiresAt  string `json:"expires_at"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Account == "" || result.NeedsLogin || result.ExpiresAt == "" {
		t.Fatalf("status = %+v", result)
	}
	for _, secret := range []string{"access-secret", "refresh-secret", "account-private"} {
		if strings.Contains(out.String()+stderr.String(), secret) {
			t.Fatalf("status exposed %s", secret)
		}
	}
}

func TestRunStatusMissingAndInvalidCommand(t *testing.T) {
	var out, stderr bytes.Buffer
	if err := run(t.Context(), []string{"status", "--store", filepath.Join(t.TempDir(), "missing")}, &out, &stderr); err != nil {
		t.Fatal(err)
	}
	var status struct {
		NeedsLogin bool `json:"needs_login"`
	}
	if err := json.Unmarshal(out.Bytes(), &status); err != nil || !status.NeedsLogin {
		t.Fatalf("status = %s, %v", out.String(), err)
	}
	if err := run(context.Background(), []string{"unknown"}, &out, &stderr); err == nil {
		t.Fatal("unknown command accepted")
	}
}
