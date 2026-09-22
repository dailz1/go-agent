package auth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type observedContext struct {
	context.Context
	once    sync.Once
	entered chan<- struct{}
}

func (c *observedContext) Done() <-chan struct{} {
	c.once.Do(func() { c.entered <- struct{}{} })
	return c.Context.Done()
}

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("event was not delivered")
		var zero T
		return zero
	}
}

func successResponse() *http.Response {
	return &http.Response{StatusCode: 200, Header: make(http.Header),
		Body: io.NopCloser(strings.NewReader(`{"access_token":"new","expires_in":3600}`))}
}

func TestRefreshSingleFlight32AndTokenDuringIO(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var posts, saves atomic.Int32
	client := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		posts.Add(1)
		close(started)
		select {
		case <-release:
			return successResponse(), nil
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	})}
	s := sourceFor(t, initialState(), RefreshConfig{ClientID: ClientID, TokenURL: Issuer + "/oauth/token",
		HTTPClient: client, Persist: func(context.Context, State) error { saves.Add(1); return nil }})
	results := make(chan error, 32)
	run := func(ctx context.Context) {
		token, err := s.Refresh(ctx, nil)
		if err == nil && token.AccessToken != "new" {
			err = errors.New("wrong token")
		}
		results <- err
	}
	go run(t.Context())
	receive(t, started)
	entered := make(chan struct{}, 31)
	for range 31 {
		go run(&observedContext{Context: t.Context(), entered: entered})
	}
	for range 31 {
		receive(t, entered)
	}
	snapshot := make(chan Token, 1)
	go func() {
		token, err := s.Token(t.Context())
		if err != nil {
			t.Error(err)
		}
		snapshot <- token
	}()
	if got := receive(t, snapshot); got.AccessToken != "old" {
		t.Fatal("unpersisted token published")
	}
	close(release)
	for range 32 {
		if err := receive(t, results); err != nil {
			t.Fatal(err)
		}
	}
	if posts.Load() != 1 || saves.Load() != 1 {
		t.Fatalf("posts=%d saves=%d", posts.Load(), saves.Load())
	}
}

func TestRefreshCanceledWaiterDoesNotCancelLeader(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	s := sourceFor(t, initialState(), RefreshConfig{ClientID: ClientID, TokenURL: Issuer + "/oauth/token",
		HTTPClient: &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
			close(started)
			select {
			case <-release:
				return successResponse(), nil
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		})}, Persist: func(context.Context, State) error { return nil }})
	leader := make(chan error, 1)
	go func() { _, err := s.Refresh(t.Context(), nil); leader <- err }()
	receive(t, started)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered := make(chan struct{}, 1)
	waiter := make(chan error, 1)
	go func() { _, err := s.Refresh(&observedContext{Context: ctx, entered: entered}, nil); waiter <- err }()
	receive(t, entered)
	cancel()
	if err := receive(t, waiter); !errors.Is(err, context.Canceled) {
		t.Fatalf("waiter error = %v", err)
	}
	close(release)
	if err := receive(t, leader); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshCanceledLeaderFailsAllWaiters(t *testing.T) {
	started := make(chan struct{})
	var posts atomic.Int32
	s := sourceFor(t, initialState(), RefreshConfig{ClientID: ClientID, TokenURL: Issuer + "/oauth/token",
		HTTPClient: &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
			posts.Add(1)
			close(started)
			<-r.Context().Done()
			return nil, r.Context().Err()
		})}, Persist: func(context.Context, State) error { t.Error("unexpected save"); return nil }})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	leader, waiter := make(chan error, 1), make(chan error, 1)
	go func() { _, err := s.Refresh(ctx, nil); leader <- err }()
	receive(t, started)
	entered := make(chan struct{}, 1)
	go func() {
		_, err := s.Refresh(&observedContext{Context: t.Context(), entered: entered}, nil)
		waiter <- err
	}()
	receive(t, entered)
	cancel()
	for _, ch := range []<-chan error{leader, waiter} {
		err := receive(t, ch)
		if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrLoginRequired) {
			t.Fatalf("error = %v", err)
		}
	}
	if _, err := s.Refresh(t.Context(), nil); !errors.Is(err, ErrLoginRequired) || posts.Load() != 1 {
		t.Fatal("unknown outcome retried")
	}
}

func TestRefreshPublishesOnlyAfterPersistence(t *testing.T) {
	saving, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	s := sourceFor(t, initialState(), RefreshConfig{ClientID: ClientID, TokenURL: Issuer + "/oauth/token",
		HTTPClient: &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) { return successResponse(), nil })},
		Persist:    func(context.Context, State) error { close(saving); <-release; return nil }})
	result := make(chan error, 1)
	go func() { _, err := s.Refresh(t.Context(), nil); result <- err }()
	receive(t, saving)
	snapshot := make(chan Token, 1)
	go func() {
		token, err := s.Token(t.Context())
		if err != nil {
			t.Error(err)
		}
		snapshot <- token
	}()
	if token := receive(t, snapshot); token.AccessToken != "old" {
		t.Fatal("published before save")
	}
	unblock()
	if err := receive(t, result); err != nil {
		t.Fatal(err)
	}
	token, err := s.Token(t.Context())
	if err != nil || token.AccessToken != "new" {
		t.Fatal("saved token not published")
	}
}
