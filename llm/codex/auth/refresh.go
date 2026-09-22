package auth

import (
	"context"
	"errors"
	"net/url"
	"sync"
	"time"
)

type refreshFlight struct {
	done  chan struct{}
	token Token
	err   error
}

// RefreshingSource serializes rotation and publishes only durably saved tokens.
// Share one instance per authorization. It starts no background goroutines.
type RefreshingSource struct {
	mu       sync.Mutex
	state    State
	cfg      RefreshConfig
	now      func() time.Time
	flight   *refreshFlight
	pending  *State
	terminal error
	rejected bool
}

var _ Source = (*RefreshingSource)(nil)

// NewRefreshingSource validates configuration without performing I/O.
func NewRefreshingSource(state State, cfg RefreshConfig) (*RefreshingSource, error) {
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	if cfg.Persist == nil {
		return nil, &Error{Stage: "token", Code: "configuration", Message: "persistence callback required"}
	}
	if state.Token.AccessToken == "" || state.Token.AccountID == "" || state.RefreshToken == "" {
		return nil, &Error{Stage: "token", Code: "credentials", LoginRequired: true, Message: "incomplete credentials"}
	}
	return &RefreshingSource{state: state, cfg: cfg, now: time.Now}, nil
}

// Token returns the last published value, even while Refresh performs I/O.
func (s *RefreshingSource) Token(ctx context.Context) (Token, error) {
	if err := ctx.Err(); err != nil {
		return Token{}, &Error{Stage: "token", Message: "snapshot canceled", Cause: err}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.Token, nil
}

// Refresh ensures the token is usable and its rotation has been persisted.
// A canceled waiter never cancels the leader; the leader owns the HTTP context.
func (s *RefreshingSource) Refresh(ctx context.Context, rejected *Token) (Token, error) {
	if err := ctx.Err(); err != nil {
		return Token{}, &Error{Stage: "refresh", Message: "refresh canceled", Cause: err}
	}
	s.mu.Lock()
	if s.terminal != nil {
		err := s.terminal
		s.mu.Unlock()
		return Token{}, err
	}
	if rejected != nil && rejected.AccessToken == s.state.Token.AccessToken &&
		rejected.AccountID == s.state.Token.AccountID {
		s.rejected = true
	}
	if flight := s.flight; flight != nil {
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return Token{}, &Error{Stage: "refresh", Message: "refresh wait canceled", Cause: ctx.Err()}
		case <-flight.done:
			return flight.token, flight.err
		}
	}
	if s.pending == nil && !s.rejected && s.state.Token.ExpiresAt.After(s.now().Add(time.Minute)) {
		token := s.state.Token
		s.mu.Unlock()
		return token, nil
	}
	flight := &refreshFlight{done: make(chan struct{})}
	s.flight = flight
	state, pending := s.state, s.pending
	s.mu.Unlock()

	next := state
	var err error
	if pending != nil {
		next = *pending
	} else {
		form := url.Values{
			"grant_type": {"refresh_token"}, "client_id": {s.cfg.ClientID},
			"refresh_token": {state.RefreshToken},
		}
		var response tokenResponse
		response, err = requestTokens(ctx, s.cfg, form)
		if err == nil {
			next, err = response.state(state, s.now())
		}
	}
	var saveErr error
	if err == nil {
		if cause := s.cfg.Persist(ctx, next); cause != nil {
			saveErr = &Error{Stage: "persist", Code: "storage", Temporary: true,
				Message: "credential persistence failed", Cause: cause}
			err = saveErr
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case saveErr != nil:
		s.pending = &next
	case err == nil:
		s.state = next
		s.pending = nil
		s.rejected = false
		flight.token = next.Token
	case errors.Is(err, ErrLoginRequired):
		s.terminal = err
	}
	flight.err = err
	s.flight = nil
	close(flight.done)
	return flight.token, flight.err
}
