package agenttest

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/dailz1/go-agent/agent"
	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

func TestNaturalAndTerminalStreamsMatchReplayInAgent(t *testing.T) {
	for _, test := range []struct {
		name      string
		chunks    []llm.Chunk
		streamErr error
	}{
		{"natural", []llm.Chunk{llm.TextDeltaChunk{Text: "done"}}, nil},
		{"terminal", []llm.Chunk{llm.TextDeltaChunk{Text: "before"}}, context.Canceled},
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

func TestPointerChunksPreserveAgentBehaviorAndRejectExport(t *testing.T) {
	for _, chunk := range []llm.Chunk{&llm.TextDeltaChunk{Text: "ignored"}, (*llm.TextDeltaChunk)(nil)} {
		request := agentRequest("x")
		baseline := NewScriptedProvider(Exchange{Method: MethodChatStream, Request: request, StreamChunks: []llm.Chunk{chunk}})
		direct, err := agent.New(baseline, tool.NewRegistry()).Run(context.Background(), "x")
		if err != nil {
			t.Fatal(err)
		}
		recorder := NewRecorder(NewScriptedProvider(Exchange{Method: MethodChatStream, Request: request, StreamChunks: []llm.Chunk{chunk}}))
		wrapped, err := agent.New(recorder, tool.NewRegistry()).Run(context.Background(), "x")
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(direct.Message, wrapped.Message) {
			t.Fatalf("pointer changed agent behavior: %#v %#v", direct.Message, wrapped.Message)
		}
		if _, err := recorder.Bytes(); !errors.Is(err, ErrUnsupportedChunk) {
			t.Fatalf("pointer Bytes = %v", err)
		}
	}
}

func agentRequest(input string) Request {
	return Request{Messages: []llm.Message{llm.UserMessage(input)}, Tools: []tool.ToolInfo{}}
}
