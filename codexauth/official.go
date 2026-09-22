package codexauth

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/dailz1/go-agent/llm/codex/auth"
)

type officialSource struct {
	mu       sync.Mutex
	token    auth.Token
	rejected bool
}

// OpenOfficialSource imports only the access token from an explicitly named
// official auth.json. It never writes the file or uses its refresh token.
func OpenOfficialSource(path string) (auth.Source, error) {
	file, err := readCredentialFile(path)
	if err != nil {
		return nil, err
	}
	if file.APIKey != "" || (file.AuthMode != "" && file.AuthMode != "chatgpt") {
		return nil, errors.New("API-key credentials require openairesponses")
	}
	if file.Tokens.AccessToken == "" {
		return nil, auth.ErrLoginRequired
	}
	token, err := auth.ParseToken(file.Tokens.AccessToken)
	if err != nil {
		return nil, err
	}
	if file.Tokens.AccountID != "" {
		if token.AccountID != "" && token.AccountID != file.Tokens.AccountID {
			return nil, &auth.Error{Stage: "token", LoginRequired: true, Message: "credential account mismatch"}
		}
		token.AccountID = file.Tokens.AccountID
	}
	if token.AccountID == "" {
		return nil, &auth.Error{Stage: "token", LoginRequired: true, Message: "credential account missing"}
	}
	return &officialSource{token: token}, nil
}

func (s *officialSource) Token(ctx context.Context) (auth.Token, error) {
	if err := ctx.Err(); err != nil {
		return auth.Token{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.token, nil
}

func (s *officialSource) Refresh(ctx context.Context, rejected *auth.Token) (auth.Token, error) {
	if err := ctx.Err(); err != nil {
		return auth.Token{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if rejected != nil && rejected.AccessToken == s.token.AccessToken && rejected.AccountID == s.token.AccountID {
		s.rejected = true
	}
	if s.rejected || !s.token.ExpiresAt.After(time.Now().Add(time.Minute)) {
		return auth.Token{}, &auth.Error{Stage: "token", LoginRequired: true, Message: "access-only credentials require login"}
	}
	return s.token, nil
}
