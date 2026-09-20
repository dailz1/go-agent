package agenttest

import (
	"context"
	"errors"
	"reflect"
	"syscall"
	"testing"
	"time"

	"github.com/dailz1/go-agent/agent"
	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

func TestRecordReplayAllChunkVariants(t *testing.T) {
	request := Request{Messages: []llm.Message{llm.UserMessage("stream")}}
	chunks := []llm.Chunk{
		llm.TextDeltaChunk{Text: "text", OutputIndex: 2}, llm.ReasoningDeltaChunk{Text: "think"},
		llm.ToolCallStartChunk{Index: 1, ID: "call", Name: "tool"}, llm.ToolCallArgsChunk{Index: 1, ID: "call", Delta: `{"x":`},
		llm.ReasoningItemChunk{OutputIndex: 3, Item: llm.ReasoningItemBlock{Type: "reasoning_item", ID: "reason", EncryptedContent: "encrypted", Summary: []string{"summary"}}},
		llm.DoneChunk{FinishReason: "tool_calls"}, llm.DoneChunk{FinishReason: "stop", Usage: &llm.Usage{InputTokens: 1, OutputTokens: 2}},
	}
	recorder := NewRecorder(NewScriptedProvider(Exchange{Method: MethodChatStream, Request: request, StreamChunks: chunks}))
	sequence, err := recorder.ChatStream(context.Background(), request.Messages, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []llm.Chunk
	for chunk, err := range sequence {
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, chunk)
	}
	if !reflect.DeepEqual(got, chunks) {
		t.Fatalf("recorded chunks = %#v", got)
	}
	bytes, err := recorder.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	replay, err := NewReplayer(bytes)
	if err != nil {
		t.Fatal(err)
	}
	sequence, err = replay.ChatStream(context.Background(), request.Messages, nil)
	if err != nil {
		t.Fatal(err)
	}
	got = nil
	for chunk, err := range sequence {
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, chunk)
	}
	if !reflect.DeepEqual(got, chunks) {
		t.Fatalf("replayed chunks = %#v", got)
	}
	if err := replay.Verify(); err != nil {
		t.Fatal(err)
	}
}

func TestReplayerStreamingFallbackWorksWithAgent(t *testing.T) {
	tools := []tool.ToolInfo{}
	request := Request{Messages: []llm.Message{llm.UserMessage("fallback")}, Tools: tools}
	response := llm.AssistantMessage("done")
	recorder := NewRecorder(NewScriptedProvider(
		Exchange{Method: MethodChatStream, Request: request, StreamOuterErr: llm.ErrStreamingNotSupported},
		Exchange{Method: MethodChat, Request: request, ChatResponse: &response},
	))
	_, _ = recorder.ChatStream(context.Background(), request.Messages, tools)
	_, _, _ = recorder.Chat(context.Background(), request.Messages, tools)
	bytes, err := recorder.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	replay, err := NewReplayer(bytes)
	if err != nil {
		t.Fatal(err)
	}
	result, err := agent.New(replay, tool.NewRegistry()).Run(context.Background(), "fallback")
	if err != nil || result.Message.Content[0].(llm.TextBlock).Text != "done" {
		t.Fatalf("Run = %#v, %v", result, err)
	}
	if err := replay.Verify(); err != nil {
		t.Fatal(err)
	}
}

func TestRecordReplayErrorTaxonomy(t *testing.T) {
	cases := []struct {
		name  string
		err   error
		check func(error) bool
	}{
		{"api", &llm.APIError{StatusCode: 429, RetryAfter: time.Second, Body: "body"}, func(err error) bool {
			var api *llm.APIError
			return errors.As(err, &api) && api.StatusCode == 429 && api.RetryAfter == time.Second && api.Body == "body"
		}},
		{"network", syscall.ECONNRESET, llm.IsNetworkError},
		{"canceled", context.Canceled, func(err error) bool { return errors.Is(err, context.Canceled) }},
		{"deadline", context.DeadlineExceeded, func(err error) bool { return errors.Is(err, context.DeadlineExceeded) }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			request := Request{Messages: []llm.Message{llm.UserMessage(test.name)}}
			recorder := NewRecorder(NewScriptedProvider(Exchange{Method: MethodChat, Request: request, ChatErr: test.err}))
			_, _, _ = recorder.Chat(context.Background(), request.Messages, nil)
			bytes, err := recorder.Bytes()
			if err != nil {
				t.Fatal(err)
			}
			replay, err := NewReplayer(bytes)
			if err != nil {
				t.Fatal(err)
			}
			_, _, got := replay.Chat(context.Background(), request.Messages, nil)
			if !test.check(got) {
				t.Fatalf("replayed error = %#v", got)
			}
		})
	}
}

func TestRecordReplayStreamCompletionAndInterruption(t *testing.T) {
	request := Request{Messages: []llm.Message{llm.UserMessage("x")}}
	complete := NewRecorder(NewScriptedProvider(Exchange{Method: MethodChatStream, Request: request, StreamChunks: []llm.Chunk{llm.ToolCallStartChunk{ID: "call", Name: "tool"}, llm.ToolCallArgsChunk{ID: "call", Delta: "{}"}}}))
	sequence, err := complete.ChatStream(context.Background(), request.Messages, nil)
	if err != nil {
		t.Fatal(err)
	}
	for range sequence {
	}
	bytes, err := complete.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewReplayer(bytes); err != nil {
		t.Fatalf("natural no-Done recording rejected: %v", err)
	}

	terminal := NewRecorder(NewScriptedProvider(Exchange{Method: MethodChatStream, Request: request, StreamChunks: []llm.Chunk{llm.TextDeltaChunk{Text: "before"}}, StreamErr: context.Canceled}))
	sequence, err = terminal.ChatStream(context.Background(), request.Messages, nil)
	if err != nil {
		t.Fatal(err)
	}
	sawTerminal := false
	for _, streamErr := range sequence {
		if streamErr != nil {
			sawTerminal = errors.Is(streamErr, context.Canceled)
		}
	}
	if !sawTerminal {
		t.Fatal("terminal stream error was not delivered")
	}
	bytes, err = terminal.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	replay, err := NewReplayer(bytes)
	if err != nil {
		t.Fatal(err)
	}
	sequence, err = replay.ChatStream(context.Background(), request.Messages, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, streamErr := range sequence {
		if streamErr != nil && !errors.Is(streamErr, context.Canceled) {
			t.Fatal(streamErr)
		}
	}

	interrupted := NewRecorder(NewScriptedProvider(Exchange{Method: MethodChatStream, Request: request, StreamChunks: []llm.Chunk{llm.TextDeltaChunk{Text: "one"}}}))
	sequence, err = interrupted.ChatStream(context.Background(), request.Messages, nil)
	if err != nil {
		t.Fatal(err)
	}
	for range sequence {
		break
	}
	bytes, err = interrupted.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewReplayer(bytes); !errors.Is(err, ErrInterruptedRecording) {
		t.Fatalf("interrupted replay = %v", err)
	}
}

func TestRecorderRejectsPointerAndActiveStream(t *testing.T) {
	request := Request{Messages: []llm.Message{llm.UserMessage("x")}}
	chunk := &llm.TextDeltaChunk{Text: "ignored by agent"}
	recorder := NewRecorder(NewScriptedProvider(Exchange{Method: MethodChatStream, Request: request, StreamChunks: []llm.Chunk{chunk}}))
	sequence, err := recorder.ChatStream(context.Background(), request.Messages, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := recorder.Bytes(); !errors.Is(err, ErrActiveRecording) {
		t.Fatalf("active Bytes = %v", err)
	}
	for got := range sequence {
		value, ok := got.(*llm.TextDeltaChunk)
		if !ok || value == nil || value.Text != chunk.Text {
			t.Fatalf("chunk dynamic form changed: %#v", got)
		}
	}
	if _, err := recorder.Bytes(); !errors.Is(err, ErrUnsupportedChunk) {
		t.Fatalf("pointer Bytes = %v", err)
	}
}

func TestReplayerRejectsUnknownVersionAndVerifiesOrder(t *testing.T) {
	if _, err := NewReplayer([]byte(`{"version":3,"exchanges":[]}`)); !errors.Is(err, ErrIncompatibleRecording) {
		t.Fatalf("version error = %v", err)
	}
	request := Request{Messages: []llm.Message{llm.UserMessage("x")}}
	response := llm.AssistantMessage("x")
	recorder := NewRecorder(NewScriptedProvider(Exchange{Method: MethodChatStream, Request: request}, Exchange{Method: MethodChat, Request: request, ChatResponse: &response}))
	sequence, err := recorder.ChatStream(context.Background(), request.Messages, nil)
	if err != nil {
		t.Fatal(err)
	}
	for range sequence {
	}
	_, _, _ = recorder.Chat(context.Background(), request.Messages, nil)
	bytes, err := recorder.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	replay, err := NewReplayer(bytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := replay.Chat(context.Background(), request.Messages, nil); !errors.Is(err, ErrScriptMismatch) {
		t.Fatalf("order error = %v", err)
	}
	if err := replay.Verify(); err == nil || !errors.Is(err, ErrUnverifiedScript) {
		t.Fatalf("Verify = %v", err)
	}
}
