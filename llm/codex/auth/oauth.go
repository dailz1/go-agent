package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/url"
	"time"
)

// PKCE binds an authorization callback to one login attempt.
type PKCE struct {
	State     string
	Verifier  string
	Challenge string
}

// NewPKCE creates independent random state and an RFC 7636 S256 verifier.
func NewPKCE() (PKCE, error) {
	var random [64]byte
	if _, err := rand.Read(random[:]); err != nil {
		return PKCE{}, &Error{Stage: "token", Code: "random", Message: "cannot generate PKCE", Cause: err}
	}
	verifier := base64.RawURLEncoding.EncodeToString(random[32:])
	hash := sha256.Sum256([]byte(verifier))
	return PKCE{State: base64.RawURLEncoding.EncodeToString(random[:32]), Verifier: verifier,
		Challenge: base64.RawURLEncoding.EncodeToString(hash[:])}, nil
}

// AuthorizationURL uses the fixed Codex browser authorization profile.
// The caller must validate the callback state before exchanging its code.
func AuthorizationURL(pkce PKCE) string {
	query := url.Values{
		"response_type": {"code"}, "client_id": {ClientID}, "redirect_uri": {RedirectURI},
		"scope": {Scopes}, "state": {pkce.State}, "code_challenge": {pkce.Challenge},
		"code_challenge_method":      {"S256"},
		"id_token_add_organizations": {"true"},
		"codex_cli_simplified_flow":  {"true"},
		"originator":                 {"go_agent"},
	}
	return Issuer + "/oauth/authorize?" + query.Encode()
}

// Exchange exchanges one authorization code without persisting it. Empty endpoint
// and client ID select the fixed profile; overrides support explicit test endpoints.
func Exchange(ctx context.Context, cfg RefreshConfig, code, verifier string) (State, error) {
	if cfg.ClientID == "" {
		cfg.ClientID = ClientID
	}
	if cfg.TokenURL == "" {
		cfg.TokenURL = Issuer + "/oauth/token"
	}
	if err := validateConfig(cfg); err != nil {
		return State{}, err
	}
	if code == "" || verifier == "" {
		return State{}, &Error{Stage: "token", Code: "configuration", Message: "code and verifier required"}
	}
	response, err := requestTokens(ctx, cfg, url.Values{
		"grant_type": {"authorization_code"}, "client_id": {cfg.ClientID}, "code": {code},
		"code_verifier": {verifier}, "redirect_uri": {RedirectURI},
	})
	if err == nil && response.RefreshToken == "" {
		err = &Error{Stage: "token", Code: "credentials", LoginRequired: true, Message: "refresh token missing"}
	}
	var state State
	if err == nil {
		state, err = response.state(State{}, time.Now())
	}
	if err != nil {
		var authErr *Error
		if errors.As(err, &authErr) {
			copy := *authErr
			copy.Stage = "token"
			err = &copy
		}
		return State{}, err
	}
	return state, nil
}
