package agenttest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"syscall"
	"testing"
	"time"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/llm/codex"
)

func TestErrorTaxonomyPersistsThroughReplay(t *testing.T) {
	generic := errors.New("generic message")
	cases := []struct {
		name  string
		err   error
		check func(*testing.T, error)
	}{
		{"api", &llm.APIError{StatusCode: 429, RetryAfter: time.Second, Body: "body"}, checkAPIError},
		{"canceled", context.Canceled, func(t *testing.T, err error) {
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v", err)
			}
		}},
		{"deadline", context.DeadlineExceeded, func(t *testing.T, err error) {
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("error = %v", err)
			}
		}},
		{"capability", llm.ErrStreamingNotSupported, func(t *testing.T, err error) {
			if !errors.Is(err, llm.ErrStreamingNotSupported) {
				t.Fatalf("error = %v", err)
			}
		}},
		{"timeout", timeoutError{}, checkNetworkError}, {"temporary", temporaryError{}, checkNetworkError},
		{"url", &url.Error{Err: plainNetworkError{}}, checkNetworkError}, {"refused", syscall.ECONNREFUSED, checkNetworkError},
		{"reset", syscall.ECONNRESET, checkNetworkError}, {"pipe", syscall.EPIPE, checkNetworkError},
		{"generic", generic, func(t *testing.T, err error) { checkGenericError(t, err, generic.Error()) }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			request := Request{Messages: []llm.Message{llm.UserMessage(test.name)}}
			recorder := NewRecorder(NewScriptedProvider(Exchange{Method: MethodChat, Request: request, ChatErr: test.err}))
			_, _, _ = recorder.Chat(t.Context(), request.Messages, nil)
			bytes, err := recorder.Bytes()
			if err != nil {
				t.Fatal(err)
			}
			replay, err := NewReplayer(bytes)
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = replay.Chat(t.Context(), request.Messages, nil)
			test.check(t, err)
		})
	}
}

func TestNetworkErrorPrecedence(t *testing.T) {
	got, err := encodeError(&url.Error{Err: timeoutError{}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != "network_timeout" {
		t.Fatalf("overlapping network error encoded as %q", got.Kind)
	}
}

func checkAPIError(t *testing.T, err error) {
	t.Helper()
	var api *llm.APIError
	if !errors.As(err, &api) || api.StatusCode != 429 || api.RetryAfter != time.Second || api.Body != "body" {
		t.Fatalf("error = %#v", err)
	}
}
func checkNetworkError(t *testing.T, err error) {
	t.Helper()
	if !llm.IsNetworkError(err) {
		t.Fatalf("not network error: %v", err)
	}
}
func checkGenericError(t *testing.T, err error, message string) {
	t.Helper()
	var api *llm.APIError
	if err == nil || err.Error() != message || llm.IsNetworkError(err) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, llm.ErrStreamingNotSupported) || errors.As(err, &api) {
		t.Fatalf("generic error changed classification: %#v", err)
	}
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return false }

type temporaryError struct{}

func (temporaryError) Error() string   { return "temporary" }
func (temporaryError) Timeout() bool   { return false }
func (temporaryError) Temporary() bool { return true }

type plainNetworkError struct{}

func (plainNetworkError) Error() string   { return "network" }
func (plainNetworkError) Timeout() bool   { return false }
func (plainNetworkError) Temporary() bool { return false }

func TestStreamErrorIdentitiesPersistThroughReplay(t *testing.T) {
	for _, sentinel := range []struct {
		name string
		err  error
	}{
		{"incomplete", llm.ErrStreamIncomplete}, {"unexpected EOF", io.ErrUnexpectedEOF},
	} {
		for _, form := range []string{"bare", "wrapped", "Codex cause"} {
			for _, position := range []string{"outer", "in-band"} {
				t.Run(sentinel.name+"/"+form+"/"+position, func(t *testing.T) {
					original := sentinel.err
					reset := time.Date(2026, 11, 28, 1, 2, 3, 4, time.UTC)
					if form == "wrapped" {
						original = fmt.Errorf("provider read: %w", original)
					}
					if form == "Codex cause" {
						original = &codex.Error{Kind: codex.KindProtocol, Code: "read_tail", RetryAt: &reset, Cause: fmt.Errorf("provider read: %w", original)}
					}
					request := Request{Messages: []llm.Message{llm.UserMessage("x")}}
					exchange := Exchange{Method: MethodChatStream, Request: request}
					if position == "outer" {
						exchange.StreamOuterErr = original
					} else {
						exchange.StreamErr = original
					}
					source := NewScriptedProvider(exchange)
					recorder := NewRecorder(source)
					seq, got := recorder.ChatStream(t.Context(), request.Messages, nil)
					if position == "in-band" {
						if got != nil {
							t.Fatal(got)
						}
						for chunk, err := range seq {
							if chunk != nil {
								t.Fatalf("unexpected chunk: %T", chunk)
							}
							got = err
						}
					}
					if got == nil || got.Error() != original.Error() {
						t.Fatalf("record error = %v", got)
					}
					if err := source.Verify(); err != nil {
						t.Fatal(err)
					}
					data, err := recorder.Bytes()
					if err != nil {
						t.Fatal(err)
					}
					replay, err := NewReplayer(data)
					if err != nil {
						t.Fatal(err)
					}
					seq, got = replay.ChatStream(t.Context(), request.Messages, nil)
					if position == "in-band" {
						if got != nil {
							t.Fatal(got)
						}
						for chunk, err := range seq {
							if chunk != nil {
								t.Fatalf("unexpected replay chunk: %T", chunk)
							}
							got = err
						}
					}
					if got == nil || got.Error() != original.Error() || !errors.Is(got, sentinel.err) || !llm.IsNetworkError(got) {
						t.Fatalf("replayed error = %v, want %v with sentinel identity", got, original)
					}
					var typed *codex.Error
					if errors.As(got, &typed) != (form == "Codex cause") {
						t.Fatalf("Codex classification changed: %v", got)
					}
					if typed != nil && (typed.Kind != codex.KindProtocol || typed.Code != "read_tail" || typed.RetryAt == nil || !typed.RetryAt.Equal(reset) || typed.Cause.Error() != "provider read: "+sentinel.err.Error()) {
						t.Fatalf("Codex metadata changed: %#v", typed)
					}
					if err := replay.Verify(); err != nil {
						t.Fatal(err)
					}
				})
			}
		}
	}
}
