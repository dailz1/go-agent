// Package auth implements non-interactive Codex OAuth credentials.
// Storage, login interaction, and environment discovery belong to callers.
package auth

import (
	"context"
	"errors"
	"net/http"
	"time"
)

const (
	ClientID    = "app_EMoamEEZ73f0CkXaXp7hrann"
	Issuer      = "https://auth.openai.com"
	RedirectURI = "http://localhost:1455/auth/callback"
	Scopes      = "openid profile email offline_access"
)

// Token is a value snapshot. ExpiresAt is a scheduling hint, not verified identity.
type Token struct {
	AccessToken string
	AccountID   string
	ExpiresAt   time.Time
}

// State is the complete state delivered to the caller's persistence callback.
type State struct {
	Token        Token
	RefreshToken string
	IDToken      string
	LastRefresh  time.Time
}

// Source owns refresh policy. Token only reads the last published snapshot.
// Refresh(nil) ensures availability; rejected identifies a token refused by a 401.
type Source interface {
	Token(context.Context) (Token, error)
	Refresh(context.Context, *Token) (Token, error)
}

// RefreshConfig supplies the token endpoint and durable storage boundary.
// Persist is mandatory for NewRefreshingSource, but is not called by Exchange.
type RefreshConfig struct {
	ClientID   string
	TokenURL   string
	HTTPClient *http.Client
	Persist    func(context.Context, State) error
}

// ErrLoginRequired indicates that this authorization cannot safely be refreshed.
var ErrLoginRequired = errors.New("codex auth: login required")

// Error preserves programmatic classification without exposing OAuth response bodies.
type Error struct {
	Stage         string
	Code          string
	Temporary     bool
	LoginRequired bool
	Message       string
	Cause         error
}

func (e *Error) Error() string {
	return "codex auth " + e.Stage + ": " + e.Message
}

func (e *Error) Unwrap() error { return e.Cause }

func (e *Error) Is(target error) bool {
	return target == ErrLoginRequired && e.LoginRequired
}
