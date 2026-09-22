package auth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

const maxTokenResponse = 1 << 20

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    *int64 `json:"expires_in"`
}

func validateConfig(cfg RefreshConfig) error {
	u, err := url.Parse(cfg.TokenURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Scheme != "https" && (u.Scheme != "http" || !loopback(u.Hostname()))) || cfg.ClientID == "" {
		return &Error{Stage: "token", Code: "configuration", Message: "invalid OAuth endpoint configuration"}
	}
	return nil
}

func loopback(host string) bool {
	return host == "localhost" || net.ParseIP(host).IsLoopback()
}

func requestTokens(ctx context.Context, cfg RefreshConfig, form url.Values) (tokenResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResponse{}, &Error{Stage: "refresh", Code: "request", Message: "cannot create token request", Cause: err}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := http.Client{Timeout: 30 * time.Second}
	if cfg.HTTPClient != nil {
		client = *cfg.HTTPClient
	}
	var connected atomic.Bool
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) { connected.Store(true) },
	}))
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if err := ctx.Err(); err != nil {
		return tokenResponse{}, &Error{Stage: "refresh", Message: "token request canceled", Cause: err}
	}
	resp, err := client.Do(req)
	if err != nil {
		var op *net.OpError
		var dns *net.DNSError
		preSend := (errors.As(err, &op) && op.Op == "dial") || errors.As(err, &dns)
		_, standardTransport := client.Transport.(*http.Transport)
		if (client.Transport == nil || standardTransport) && !connected.Load() {
			preSend = true
		}
		return tokenResponse{}, &Error{Stage: "refresh", Code: "transport", Temporary: preSend,
			LoginRequired: !preSend, Message: "token request failed; outcome may be unknown", Cause: err}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenResponse+1))
	if err != nil {
		return tokenResponse{}, &Error{Stage: "refresh", Code: "unknown_outcome", LoginRequired: true,
			Message: "token response interrupted", Cause: err}
	}
	if len(data) > maxTokenResponse {
		return tokenResponse{}, &Error{Stage: "refresh", Code: "unknown_outcome", LoginRequired: true,
			Message: "token response exceeds limit"}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var oauth struct {
			Error json.RawMessage `json:"error"`
		}
		var code string
		if json.Unmarshal(data, &oauth) == nil {
			if json.Unmarshal(oauth.Error, &code) != nil {
				var detail struct {
					Code string `json:"code"`
				}
				if json.Unmarshal(oauth.Error, &detail) == nil {
					code = detail.Code
				}
			}
		}
		login := code == "invalid_grant" || code == "token_revoked" || code == "refresh_token_reused" ||
			code == "refresh_token_expired" || code == "refresh_token_invalidated" || resp.StatusCode == 401
		return tokenResponse{}, &Error{Stage: "refresh", Code: code, LoginRequired: login,
			Temporary: !login && (resp.StatusCode == 429 || resp.StatusCode >= 500),
			Message:   "OAuth token endpoint rejected request"}
	}
	var result tokenResponse
	if err := json.Unmarshal(data, &result); err != nil {
		return tokenResponse{}, &Error{Stage: "refresh", Code: "unknown_outcome", LoginRequired: true,
			Message: "invalid token response", Cause: err}
	}
	return result, nil
}

func (r tokenResponse) state(old State, now time.Time) (State, error) {
	if r.AccessToken == "" {
		return State{}, &Error{Stage: "refresh", Code: "unknown_outcome", LoginRequired: true,
			Message: "token response omitted access token"}
	}
	token := Token{AccessToken: r.AccessToken}
	if strings.Count(r.AccessToken, ".") == 2 {
		parsed, err := ParseToken(r.AccessToken)
		if err != nil {
			return State{}, err
		}
		token = parsed
	}
	idToken := old.IDToken
	if r.IDToken != "" {
		idToken = r.IDToken
		id, err := ParseToken(r.IDToken)
		if err != nil {
			return State{}, err
		}
		if token.AccountID != "" && id.AccountID != "" && token.AccountID != id.AccountID {
			return State{}, &Error{Stage: "refresh", Code: "account_mismatch", LoginRequired: true, Message: "account changed"}
		}
		if token.AccountID == "" {
			token.AccountID = id.AccountID
		}
	}
	if old.Token.AccountID != "" {
		if token.AccountID != "" && token.AccountID != old.Token.AccountID {
			return State{}, &Error{Stage: "refresh", Code: "account_mismatch", LoginRequired: true, Message: "account changed"}
		}
		token.AccountID = old.Token.AccountID
	}
	if token.AccountID == "" {
		return State{}, &Error{Stage: "refresh", Code: "account_missing", LoginRequired: true, Message: "account claim missing"}
	}
	if r.ExpiresIn != nil {
		if *r.ExpiresIn < 0 || *r.ExpiresIn > int64((1<<63-1)/time.Second) {
			return State{}, &Error{Stage: "refresh", Code: "unknown_outcome", LoginRequired: true, Message: "invalid token expiry"}
		}
		token.ExpiresAt = now.Add(time.Duration(*r.ExpiresIn) * time.Second)
	}
	refresh := old.RefreshToken
	if r.RefreshToken != "" {
		refresh = r.RefreshToken
	}
	return State{Token: token, RefreshToken: refresh, IDToken: idToken, LastRefresh: now}, nil
}
