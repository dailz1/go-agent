package agenttest

import (
	"context"
	"iter"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

func TestRecorderReservesBeforeCanonicalizingRequest(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	recorder := NewRecorder(orderedProvider{})
	firstRequest := []llm.Message{llm.UserMessage("first")}
	secondRequest := []llm.Message{llm.UserMessage("second")}
	firstDone := make(chan struct{})
	go func() {
		_, _, _ = recorder.Chat(context.Background(), firstRequest, nil, func(*llm.Options) { close(entered); <-release })
		close(firstDone)
	}()
	await(t, entered)
	if _, _, err := recorder.Chat(context.Background(), secondRequest, nil); err != nil {
		t.Fatal(err)
	}
	close(release)
	await(t, firstDone)
	bytes, err := recorder.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	replay, err := NewReplayer(bytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := replay.Chat(context.Background(), firstRequest, nil, llm.WithModel("")); err != nil {
		t.Fatalf("first replay = %v", err)
	}
	if _, _, err := replay.Chat(context.Background(), secondRequest, nil); err != nil {
		t.Fatalf("second replay = %v", err)
	}
}

type orderedProvider struct{}

func (orderedProvider) Name() string { return "ordered" }
func (orderedProvider) Chat(context.Context, []llm.Message, []tool.ToolInfo, ...llm.Option) (*llm.Message, *llm.Usage, error) {
	return nil, nil, nil
}
func (orderedProvider) ChatStream(context.Context, []llm.Message, []tool.ToolInfo, ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	return nil, llm.ErrStreamingNotSupported
}
