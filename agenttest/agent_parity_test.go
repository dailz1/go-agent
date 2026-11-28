package agenttest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"testing"
	"testing/synctest"
	"time"

	"github.com/dailz1/go-agent/agent"
	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/llm/codex"
	"github.com/dailz1/go-agent/tool"
)

func TestNaturalAndTerminalStreamsMatchReplayInAgent(t *testing.T) {
	for _, test := range []struct {
		name      string
		chunks    []llm.Chunk
		streamErr error
	}{
		{"natural", []llm.Chunk{&llm.TextDeltaChunk{Text: "done"}}, nil},
		{"terminal", []llm.Chunk{&llm.TextDeltaChunk{Text: "before"}}, context.Canceled},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := agentRequest("x")
			recorder := NewRecorder(NewScriptedProvider(Exchange{Method: MethodChatStream, Request: request, StreamChunks: test.chunks, StreamErr: test.streamErr}))
			direct, directErr := agent.New(recorder, tool.NewRegistry()).Run(context.Background(), "x")
			bytes, err := recorder.Bytes()
			if err != nil {
				t.Fatal(err)
			}
			replay, err := NewReplayer(bytes)
			if err != nil {
				t.Fatal(err)
			}
			replayed, replayErr := agent.New(replay, tool.NewRegistry()).Run(context.Background(), "x")
			if (directErr == nil) != (replayErr == nil) || (directErr != nil && !errors.Is(replayErr, context.Canceled)) {
				t.Fatalf("errors direct=%v replay=%v", directErr, replayErr)
			}
			if directErr == nil && !reflect.DeepEqual(direct.Message, replayed.Message) {
				t.Fatalf("messages differ: %#v %#v", direct.Message, replayed.Message)
			}
		})
	}
}

func TestPointerChunksPreserveAgentBehaviorAndExport(t *testing.T) {
	request := agentRequest("x")
	baseline := NewScriptedProvider(Exchange{Method: MethodChatStream, Request: request, StreamChunks: []llm.Chunk{&llm.TextDeltaChunk{Text: "ignored"}}})
	direct, err := agent.New(baseline, tool.NewRegistry()).Run(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	recorder := NewRecorder(NewScriptedProvider(Exchange{Method: MethodChatStream, Request: request, StreamChunks: []llm.Chunk{&llm.TextDeltaChunk{Text: "ignored"}}}))
	wrapped, err := agent.New(recorder, tool.NewRegistry()).Run(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(direct.Message, wrapped.Message) {
		t.Fatalf("pointer changed agent behavior: %#v %#v", direct.Message, wrapped.Message)
	}
	if _, err := recorder.Bytes(); err != nil {
		t.Fatalf("pointer Bytes = %v", err)
	}
}

func agentRequest(input string) Request {
	return Request{Messages: []llm.Message{llm.UserMessage(input)}, Tools: []tool.ToolInfo{}}
}

func TestAgentStreamErrorRetryMatchesReplay(t *testing.T) {
	for _, sentinel := range []struct {
		name string
		err  error
	}{
		{"incomplete", llm.ErrStreamIncomplete}, {"unexpected EOF", io.ErrUnexpectedEOF},
	} {
		for _, form := range []string{"bare", "wrapped", "Codex cause"} {
			for _, position := range []string{"outer", "in-band"} {
				t.Run(sentinel.name+"/"+form+"/"+position, func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						original := sentinel.err
						if form == "wrapped" {
							original = fmt.Errorf("provider read: %w", original)
						}
						if form == "Codex cause" {
							original = &codex.Error{Kind: codex.KindProtocol, Cause: fmt.Errorf("provider read: %w", original)}
						}
						request := agentRequest("x")
						failed := Exchange{Method: MethodChatStream, Request: request}
						if position == "outer" {
							failed.StreamOuterErr = original
						} else {
							failed.StreamErr = original
						}
						source := NewScriptedProvider(
							failed,
							Exchange{Method: MethodChatStream, Request: request, StreamChunks: []llm.Chunk{&llm.TextDeltaChunk{Text: "ok"}, &llm.DoneChunk{FinishReason: "stop"}}},
						)
						run := func(provider llm.Provider) *agent.RunResult {
							t.Helper()
							var notifications []agent.RetryInfo
							runtime := agent.New(provider, tool.NewRegistry(), agent.WithRetryConfig(agent.AgentRetryConfig{
								MaxRetries: 1, BaseDelay: time.Nanosecond, MaxDelay: time.Nanosecond,
								OnRetry: func(info agent.RetryInfo) { notifications = append(notifications, info) },
							}))
							result, err := runtime.Run(t.Context(), "x")
							if err != nil {
								t.Fatal(err)
							}
							if len(result.Retries) != 1 || len(notifications) != 1 {
								t.Fatalf("retries=%v callbacks=%v", result.Retries, notifications)
							}
							info := result.Retries[0]
							if info.Attempt != 2 || info.MaxAttempts != 2 || info.Reason != llm.SanitizeRetryReason(original) || !errors.Is(info.Err, sentinel.err) || !llm.IsNetworkError(info.Err) {
								t.Fatalf("retry = %#v", info)
							}
							if notifications[0].Delay != info.Delay || notifications[0].Attempt != info.Attempt || notifications[0].MaxAttempts != info.MaxAttempts || notifications[0].Reason != info.Reason || !errors.Is(notifications[0].Err, sentinel.err) {
								t.Fatalf("callback differs from retry: %#v / %#v", notifications[0], info)
							}
							var typed *codex.Error
							if errors.As(info.Err, &typed) != (form == "Codex cause") {
								t.Fatalf("Codex classification changed: %v", info.Err)
							}
							return result
						}
						recorder := NewRecorder(source)
						direct := run(recorder)
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
						replayRecorder := NewRecorder(replay)
						replayed := run(replayRecorder)
						if !reflect.DeepEqual(direct.Message, replayed.Message) || !reflect.DeepEqual(direct.History, replayed.History) {
							t.Fatalf("agent result differs: %#v / %#v", direct, replayed)
						}
						first, second := direct.Retries[0], replayed.Retries[0]
						if first.Attempt != second.Attempt || first.MaxAttempts != second.MaxAttempts || first.Reason != second.Reason || first.Err.Error() != second.Err.Error() {
							t.Fatalf("retry metadata changed: %#v / %#v", first, second)
						}
						// Delay is sampled per run and is deliberately not compared across runs.
						replayData, err := replayRecorder.Bytes()
						if err != nil {
							t.Fatal(err)
						}
						for _, recording := range [][]byte{data, replayData} {
							wire, err := decodeRecording(recording)
							if err != nil {
								t.Fatal(err)
							}
							if len(wire.Exchanges) != 2 {
								t.Fatalf("requests = %d, want 2", len(wire.Exchanges))
							}
							for _, exchange := range wire.Exchanges {
								if exchange.Method != MethodChatStream {
									t.Fatalf("method = %s", exchange.Method)
								}
							}
						}
						if err := replay.Verify(); err != nil {
							t.Fatal(err)
						}
					})
				})
			}
		}
	}
}
