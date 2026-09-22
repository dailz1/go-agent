package codex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/llm/codex/auth"
)

func TestProvider401UsesRejectedToken(t *testing.T) {
	var requests atomic.Int32
	source := &testSource{refresh: func(rejected *auth.Token) (auth.Token, error) {
		if rejected == nil {
			return auth.Token{AccessToken: "old", AccountID: "account"}, nil
		}
		if rejected.AccessToken != "old" || rejected.AccountID != "account" {
			t.Errorf("rejected=%+v", rejected)
		}
		return auth.Token{AccessToken: "new", AccountID: "account"}, nil
	}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			w.WriteHeader(401)
			return
		}
		if r.Header.Get("Authorization") != "Bearer new" {
			t.Error("did not use refreshed token")
		}
		fmt.Fprint(w, sse(emptyCompletion))
	}))
	defer server.Close()
	if _, _, err := NewProvider("m", WithAuthSource(source), WithBaseURL(server.URL)).Chat(t.Context(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 2 || source.refreshes.Load() != 2 {
		t.Fatal("wrong recovery count")
	}
}

func TestProviderAuthFailurePreventsInference(t *testing.T) {
	for name, failure := range map[string]error{
		"login":   auth.ErrLoginRequired,
		"persist": &auth.Error{Stage: "persist", Cause: io.ErrClosedPipe},
		"cancel":  context.Canceled,
	} {
		t.Run(name, func(t *testing.T) {
			var requests atomic.Int32
			source := &testSource{refresh: func(*auth.Token) (auth.Token, error) { return auth.Token{}, failure }}
			client := &http.Client{Transport: roundTripper(func(*http.Request) (*http.Response, error) {
				requests.Add(1)
				return nil, errors.New("unexpected inference")
			})}
			_, _, err := NewProvider("m", WithAuthSource(source), WithHTTPClient(client)).Chat(t.Context(), nil, nil)
			var typed *Error
			if !errors.As(err, &typed) || typed.Kind != KindAuth || !errors.Is(err, failure) || requests.Load() != 0 {
				t.Fatalf("err=%v requests=%d", err, requests.Load())
			}
		})
	}
}

func TestProviderRejectsAccountChange(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(401) }))
	defer server.Close()
	source := &testSource{refresh: func(rejected *auth.Token) (auth.Token, error) {
		if rejected == nil {
			return auth.Token{AccessToken: "a", AccountID: "first"}, nil
		}
		return auth.Token{AccessToken: "b", AccountID: "second"}, nil
	}}
	_, _, err := NewProvider("m", WithAuthSource(source), WithBaseURL(server.URL)).Chat(t.Context(), nil, nil)
	if !errors.Is(err, auth.ErrLoginRequired) || requests.Load() != 1 {
		t.Fatalf("err=%v requests=%d", err, requests.Load())
	}
}

func TestProviderRedirectDoesNotLeakOrMutateClient(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer server.Close()
	var callback atomic.Int32
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { callback.Add(1); return nil }}
	_, _, err := NewProvider("m", WithAuthSource(&testSource{}), WithBaseURL(server.URL), WithHTTPClient(client)).Chat(t.Context(), nil, nil)
	var api *llm.APIError
	if !errors.As(err, &api) || api.StatusCode != 307 || redirected.Load() != 0 || callback.Load() != 0 {
		t.Fatalf("err=%v", err)
	}
	if err := client.CheckRedirect(nil, nil); err != nil || callback.Load() != 1 {
		t.Fatal("caller client mutated")
	}
}

func TestProviderCancellationClosesBlockedStream(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	started := make(chan struct{})
	closed := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sse(`{"type":"response.output_text.delta","output_index":0,"content_index":0,"item_id":"m","delta":"x"}`))
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(closed)
	}))
	defer server.Close()
	p := NewProvider("m", WithAuthSource(&testSource{}), WithBaseURL(server.URL))
	seq, err := p.ChatStream(ctx, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got error
	for chunk, err := range seq {
		if err != nil {
			got = err
			continue
		}
		if _, ok := chunk.(llm.TextDeltaChunk); ok {
			<-started
			cancel()
		}
		if _, ok := chunk.(llm.DoneChunk); ok {
			t.Fatal("done after cancellation")
		}
	}
	if !errors.Is(got, context.Canceled) {
		t.Fatalf("error=%v", got)
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("server connection remained open")
	}
}

func TestProviderNetworkErrorChain(t *testing.T) {
	network := &net.OpError{Op: "dial", Net: "tcp", Err: io.ErrUnexpectedEOF}
	client := &http.Client{Transport: roundTripper(func(*http.Request) (*http.Response, error) { return nil, network })}
	_, _, err := NewProvider("m", WithAuthSource(&testSource{}), WithHTTPClient(client)).Chat(t.Context(), nil, nil)
	var api *llm.APIError
	var got *net.OpError
	if !errors.As(err, &got) || !errors.Is(err, io.ErrUnexpectedEOF) || errors.As(err, &api) {
		t.Fatalf("error=%v", err)
	}
}

func TestStreamQuotaDoesNotInventHTTPStatus(t *testing.T) {
	var got error
	for _, err := range scanResponse(t.Context(), strings.NewReader(sse(`{"type":"error","error":{"code":"usage_limit_reached","resets_at":"2030-01-01T00:00:00Z"}}`))) {
		got = err
	}
	var api *llm.APIError
	var typed *Error
	if errors.As(got, &api) || !errors.As(got, &typed) || typed.Kind != KindQuota || typed.RetryAt == nil {
		t.Fatalf("error=%v", got)
	}
}
