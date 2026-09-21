package main

import (
	"context"
	"errors"
	"iter"
	"testing"

	"github.com/dailz1/go-agent/agent"
	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

type recoveryProvider struct {
	calls int
}

func (*recoveryProvider) Name() string { return "recovery-test" }
func (*recoveryProvider) ChatStream(context.Context, []llm.Message, []tool.ToolInfo, ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	return nil, llm.ErrStreamingNotSupported
}
func (p *recoveryProvider) Chat(context.Context, []llm.Message, []tool.ToolInfo, ...llm.Option) (*llm.Message, *llm.Usage, error) {
	p.calls++
	if p.calls == 1 {
		return nil, nil, context.Canceled
	}
	reply := llm.AssistantMessage("recovered")
	return &reply, nil, nil
}

func TestRunOnThreadResumesLazyIncomplete(t *testing.T) {
	p := &recoveryProvider{}
	a := agent.New(p, tool.NewRegistry(), agent.WithStore(store.NewMemory()))
	if _, err := a.RunThread(context.Background(), "t", "old"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	runOnThread(context.Background(), a, "t", "new")
	if p.calls != 2 {
		t.Fatalf("provider calls = %d; lazy incomplete error did not resume old input", p.calls)
	}
	if _, err := a.ResumeThread(context.Background(), "t"); !errors.Is(err, agent.ErrNothingToResume) {
		t.Fatalf("old run was not completed: %v", err)
	}
}
