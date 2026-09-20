package agenttest

import (
	"context"
	"errors"
	"net/url"
	"syscall"
	"testing"
	"time"

	"github.com/dailz1/go-agent/llm"
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
	if got := encodeError(&url.Error{Err: timeoutError{}}).Kind; got != "network_timeout" {
		t.Fatalf("overlapping network error encoded as %q", got)
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
