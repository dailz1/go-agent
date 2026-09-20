package agenttest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dailz1/go-agent/llm"
)

func TestScriptedProviderRejectsOverlappingCalls(t *testing.T) {
	request := Request{Messages: []llm.Message{llm.UserMessage("x")}}
	for _, test := range []struct {
		name     string
		exchange Exchange
		call     func(*ScriptedProvider, []llm.Option) error
	}{
		{"chat", Exchange{Method: MethodChat, Request: request}, func(p *ScriptedProvider, options []llm.Option) error {
			_, _, err := p.Chat(context.Background(), request.Messages, nil, options...)
			return err
		}},
		{"outer stream", Exchange{Method: MethodChatStream, Request: request, StreamOuterErr: llm.ErrStreamingNotSupported}, func(p *ScriptedProvider, options []llm.Option) error {
			_, err := p.ChatStream(context.Background(), request.Messages, nil, options...)
			return err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			blocking := func(*llm.Options) { close(entered); <-release }
			provider := NewScriptedProvider(test.exchange, test.exchange)
			first := make(chan error, 1)
			go func() { first <- test.call(provider, []llm.Option{blocking}) }()
			await(t, entered)
			if err := test.call(provider, nil); !errors.Is(err, ErrConcurrentScriptUse) {
				t.Fatalf("overlap error = %v", err)
			}
			close(release)
			if err := awaitError(t, first); err != nil && !errors.Is(err, llm.ErrStreamingNotSupported) {
				t.Fatal(err)
			}
			if err := provider.Verify(); err == nil {
				t.Fatal("losing call consumed a script step")
			}
		})
	}
}

func TestReplayerRejectsOverlappingCalls(t *testing.T) {
	request := Request{Messages: []llm.Message{llm.UserMessage("x")}}
	recorder := NewRecorder(NewScriptedProvider(Exchange{Method: MethodChat, Request: request}, Exchange{Method: MethodChat, Request: request}))
	_, _, _ = recorder.Chat(context.Background(), request.Messages, nil)
	_, _, _ = recorder.Chat(context.Background(), request.Messages, nil)
	bytes, err := recorder.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	replay, err := NewReplayer(bytes)
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	first := make(chan error, 1)
	go func() {
		_, _, err := replay.Chat(context.Background(), request.Messages, nil, func(*llm.Options) { close(entered); <-release })
		first <- err
	}()
	await(t, entered)
	if _, _, err := replay.Chat(context.Background(), request.Messages, nil); !errors.Is(err, ErrConcurrentScriptUse) {
		t.Fatalf("overlap error = %v", err)
	}
	close(release)
	if err := awaitError(t, first); err != nil {
		t.Fatal(err)
	}
}

func await(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for signal")
	}
}
func awaitError(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for result")
		return nil
	}
}
