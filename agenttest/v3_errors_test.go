package agenttest

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"reflect"
	"syscall"
	"testing"
	"time"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/llm/codex"
	"github.com/dailz1/go-agent/llm/codex/auth"
)

func TestRecordReplayV3ErrorLocations(t *testing.T) {
	reset := time.Date(2026, 9, 22, 1, 2, 3, 123456789, time.UTC)
	cases := map[string]error{
		"quota":             &codex.Error{Kind: "quota", Code: "usage_limit_reached", RetryAt: &reset, Cause: &llm.APIError{StatusCode: 429, Body: "quota", RetryAfter: time.Second, NonRetryable: true}},
		"rate":              &llm.APIError{StatusCode: 429, Body: "rate", RetryAfter: 2 * time.Second},
		"auth":              &codex.Error{Kind: "auth", Code: "unauthorized", Cause: &llm.APIError{StatusCode: 401}},
		"invalid_grant":     &codex.Error{Kind: "auth", Cause: &auth.Error{Stage: "refresh", Code: "invalid_grant", LoginRequired: true, Message: "login again", Cause: errors.New("rejected")}},
		"temporary":         &auth.Error{Stage: "refresh", Temporary: true, Code: "unavailable", Message: "try again"},
		"persist":           &auth.Error{Stage: "persist", Message: "save failed", Cause: errors.New("disk")},
		"login_required":    auth.ErrLoginRequired,
		"canceled":          &auth.Error{Stage: "refresh", Cause: context.Canceled},
		"deadline":          &codex.Error{Kind: "auth", Cause: &auth.Error{Stage: "token", Cause: context.DeadlineExceeded}},
		"timeout":           &codex.Error{Kind: "protocol", Cause: timeoutError{}},
		"network_temporary": &auth.Error{Stage: "refresh", Cause: temporaryError{}},
		"network_url":       &codex.Error{Kind: "protocol", Cause: &url.Error{Op: "POST", URL: "http://localhost", Err: plainNetworkError{}}},
		"network_errno":     &auth.Error{Stage: "refresh", Cause: syscall.ECONNRESET},
		"capability":        &codex.Error{Kind: "capability", Cause: llm.ErrStreamingNotSupported},
		"protocol":          &codex.Error{Kind: "protocol", Code: "missing_completed"},
		"wrapped":           fmt.Errorf("request: %w", &codex.Error{Kind: "auth", Cause: fmt.Errorf("token: %w", &auth.Error{Stage: "refresh", Cause: context.Canceled})}),
	}
	for name, want := range cases {
		for _, location := range []string{"chat", "outer", "yielded"} {
			t.Run(name+"/"+location, func(t *testing.T) {
				exchange := Exchange{Method: MethodChat, ChatErr: want}
				if location != "chat" {
					exchange = Exchange{Method: MethodChatStream}
					if location == "outer" {
						exchange.StreamOuterErr = want
					} else {
						exchange.StreamChunks = []llm.Chunk{llm.TextDeltaChunk{Text: "before"}}
						exchange.StreamErr = want
					}
				}
				recorder := NewRecorder(NewScriptedProvider(exchange))
				checkV3Classification(t, callErrorLocation(t, recorder, location), want)
				data, err := recorder.Bytes()
				if err != nil {
					t.Fatal(err)
				}
				replay, err := NewReplayer(data)
				if err != nil {
					t.Fatal(err)
				}
				checkV3Classification(t, callErrorLocation(t, replay, location), want)
				if err := replay.Verify(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func callErrorLocation(t *testing.T, provider llm.Provider, location string) error {
	t.Helper()
	if location == "chat" {
		_, _, err := provider.Chat(t.Context(), nil, nil)
		return err
	}
	seq, err := provider.ChatStream(t.Context(), nil, nil)
	if location == "outer" {
		if seq != nil {
			t.Fatal("outer failure returned an iterator")
		}
		return err
	}
	if err != nil {
		t.Fatalf("yielded error moved outside stream: %v", err)
	}
	chunks := 0
	var terminal error
	for chunk, err := range seq {
		if err != nil {
			if chunks != 1 || terminal != nil || chunk != nil {
				t.Fatal("stream error position changed")
			}
			terminal = err
			continue
		}
		if terminal != nil || chunk != (llm.TextDeltaChunk{Text: "before"}) {
			t.Fatalf("unexpected chunk: %#v", chunk)
		}
		chunks++
	}
	if terminal == nil {
		t.Fatal("missing terminal error")
	}
	return terminal
}

func checkV3Classification(t *testing.T, got, want error) {
	t.Helper()
	if got == nil || want == nil {
		if got != want {
			t.Fatal("cause presence changed")
		}
		return
	}
	var gc, wc *codex.Error
	if errors.As(got, &gc) != errors.As(want, &wc) {
		t.Fatalf("Codex classification changed: %T -> %T", want, got)
	}
	if wc != nil && (gc.Kind != wc.Kind || gc.Code != wc.Code || !reflect.DeepEqual(gc.RetryAt, wc.RetryAt)) {
		t.Fatalf("Codex fields changed: %#v -> %#v", wc, gc)
	}
	var ga, wa *auth.Error
	gotAuth, wantAuth := errors.As(got, &ga), errors.As(want, &wa)
	if want == auth.ErrLoginRequired {
		if got != want && (!gotAuth || ga.Stage != "token" || !ga.LoginRequired) {
			t.Fatalf("login sentinel reconstruction = %#v", got)
		}
	} else if gotAuth != wantAuth {
		t.Fatalf("auth classification changed: %T -> %T", want, got)
	} else if wa != nil && (ga.Stage != wa.Stage || ga.Code != wa.Code || ga.Temporary != wa.Temporary || ga.LoginRequired != wa.LoginRequired || ga.Message != wa.Message) {
		t.Fatalf("auth fields changed: %#v -> %#v", wa, ga)
	}
	var gp, wp *llm.APIError
	if errors.As(got, &gp) != errors.As(want, &wp) || !reflect.DeepEqual(gp, wp) {
		t.Fatalf("API fields changed: %#v -> %#v", wp, gp)
	}
	if wp != nil && gp.Retryable() != wp.Retryable() {
		t.Fatal("API retryability changed")
	}
	if llm.IsRetryableError(got) != llm.IsRetryableError(want) || llm.IsNetworkError(got) != llm.IsNetworkError(want) {
		t.Fatal("retry or network classification changed")
	}
	for _, sentinel := range []error{context.Canceled, context.DeadlineExceeded, auth.ErrLoginRequired, llm.ErrStreamingNotSupported, syscall.ECONNRESET} {
		if errors.Is(got, sentinel) != errors.Is(want, sentinel) {
			t.Fatalf("sentinel classification changed: %v", sentinel)
		}
	}
	var gn, wn net.Error
	if errors.As(want, &wn) && (wn.Timeout() || wn.Temporary()) {
		if !errors.As(got, &gn) || gn.Timeout() != wn.Timeout() || gn.Temporary() != wn.Temporary() {
			t.Fatal("network flags changed")
		}
	}
	if wc != nil {
		checkV3Classification(t, gc.Cause, wc.Cause)
	} else if wa != nil {
		checkV3Classification(t, ga.Cause, wa.Cause)
	} else if wp == nil && wn == nil && want != auth.ErrLoginRequired && got.Error() != want.Error() {
		t.Fatal("leaf error message changed")
	}
}

func TestRecorderV3RejectsUnfaithfulWrappers(t *testing.T) {
	cycle := &codex.Error{Kind: "auth"}
	cycle.Cause = cycle
	indirect := &auth.Error{Stage: "token"}
	indirect.Cause = fmt.Errorf("cycle: %w", indirect)
	var nilCodex *codex.Error
	for name, err := range map[string]error{
		"cycle":            cycle,
		"indirect cycle":   indirect,
		"typed nil":        nilCodex,
		"unknown category": &codex.Error{Kind: "unknown"},
		"unknown stage":    &auth.Error{Stage: "login"},
		"codex child":      &codex.Error{Kind: "auth", Cause: &codex.Error{Kind: "protocol"}},
		"auth child":       &auth.Error{Stage: "refresh", Cause: &auth.Error{Stage: "token"}},
		"auth codex":       &auth.Error{Stage: "refresh", Cause: &codex.Error{Kind: "auth"}},
		"deep":             &codex.Error{Kind: "auth", Cause: &auth.Error{Stage: "refresh", Cause: &auth.Error{Stage: "token", Cause: context.Canceled}}},
		"joined":           errors.Join(&codex.Error{Kind: "protocol"}, context.Canceled),
		"joined cause":     &codex.Error{Kind: "protocol", Cause: errors.Join(context.Canceled, syscall.ECONNRESET)},
	} {
		t.Run(name, func(t *testing.T) {
			for _, location := range []string{"chat", "outer", "yielded"} {
				exchange := Exchange{Method: MethodChat, ChatErr: err}
				if location == "outer" {
					exchange = Exchange{Method: MethodChatStream, StreamOuterErr: err}
				}
				if location == "yielded" {
					exchange = Exchange{Method: MethodChatStream, StreamChunks: []llm.Chunk{llm.TextDeltaChunk{Text: "before"}}, StreamErr: err}
				}
				recorder := NewRecorder(NewScriptedProvider(exchange))
				if got := callErrorLocation(t, recorder, location); got != err {
					t.Fatal("recorder changed provider error")
				}
				if data, err := recorder.Bytes(); err == nil || data != nil {
					t.Fatal("unfaithful error recording accepted")
				}
			}
		})
	}
}

type cyclicErrorSlice []error

func (cyclicErrorSlice) Error() string   { return "cyclic error" }
func (e cyclicErrorSlice) Unwrap() error { return e[0] }

func TestRecorderV3RejectsNonComparableCycle(t *testing.T) {
	cycle := make(cyclicErrorSlice, 1)
	cycle[0] = cycle
	failure := &auth.Error{Stage: "refresh", Cause: cycle}
	completed := make(chan error, 1)
	go func() {
		recorder := NewRecorder(NewScriptedProvider(Exchange{Method: MethodChat, ChatErr: failure}))
		recorder.Chat(t.Context(), nil, nil)
		_, err := recorder.Bytes()
		completed <- err
	}()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	select {
	case err := <-completed:
		if err == nil {
			t.Fatal("cyclic recording accepted")
		}
	case <-ctx.Done():
		t.Fatal("recording did not reject a cyclic non-comparable error")
	}
}
