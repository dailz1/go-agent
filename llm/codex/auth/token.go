package auth

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

// ParseToken reads unverified JWT claims only for expiry scheduling and account
// routing. It does not authenticate the issuer, signature, or identity. Missing
// claims remain zero values; callers must not invent an account ID.
func ParseToken(accessToken string) (Token, error) {
	parts := strings.Split(accessToken, ".")
	if len(parts) != 3 || len(accessToken) > maxTokenResponse {
		return Token{}, &Error{Stage: "token", Code: "invalid_token", LoginRequired: true, Message: "invalid JWT"}
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Token{}, &Error{Stage: "token", Code: "invalid_token", LoginRequired: true, Message: "invalid JWT encoding", Cause: err}
	}
	var claims *struct {
		Exp       int64  `json:"exp"`
		AccountID string `json:"chatgpt_account_id"`
		Auth      struct {
			AccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if err := json.Unmarshal(data, &claims); err != nil {
		return Token{}, &Error{Stage: "token", Code: "invalid_token", LoginRequired: true, Message: "invalid JWT claims", Cause: err}
	}
	if claims == nil {
		return Token{}, &Error{Stage: "token", Code: "invalid_token", LoginRequired: true, Message: "JWT claims must be an object"}
	}
	account := claims.Auth.AccountID
	if account == "" {
		account = claims.AccountID
	}
	if claims.Auth.AccountID != "" && claims.AccountID != "" && claims.Auth.AccountID != claims.AccountID {
		return Token{}, &Error{Stage: "token", Code: "account_mismatch", LoginRequired: true, Message: "conflicting account claims"}
	}
	token := Token{AccessToken: accessToken, AccountID: account}
	if claims.Exp > 0 {
		token.ExpiresAt = time.Unix(claims.Exp, 0)
	}
	return token, nil
}
