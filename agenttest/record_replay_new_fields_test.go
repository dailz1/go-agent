package agenttest

import (
	"context"
	"reflect"
	"testing"

	"github.com/dailz1/go-agent/llm"
)

func TestRecordReplayNewOptionFields(t *testing.T) {
	seed := int64(42)
	topP := 0.9
	parallel := false
	mct := int64(8192)
	presence := 0.5
	frequency := -0.25
	newOpts := []llm.Option{
		llm.WithMaxCompletionTokens(mct), llm.WithTopP(topP), llm.WithSeed(seed),
		llm.WithPresencePenalty(presence), llm.WithFrequencyPenalty(frequency),
		llm.WithParallelToolCalls(parallel), llm.WithToolChoiceAuto(),
	}
	want := llm.ApplyOptions(newOpts)
	request := Request{Messages: []llm.Message{llm.UserMessage("stream")}, Options: cloneOptions(want)}
	chunks := []llm.Chunk{
		llm.DoneChunk{FinishReason: "stop", Usage: &llm.Usage{InputTokens: 1, OutputTokens: 2, CachedTokens: 1}},
	}
	recorder := NewRecorder(NewScriptedProvider(Exchange{Method: MethodChatStream, Request: request, StreamChunks: chunks}))
	sequence, err := recorder.ChatStream(context.Background(), request.Messages, nil, newOpts...)
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
	sequence, err = replay.ChatStream(context.Background(), request.Messages, nil, newOpts...)
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
