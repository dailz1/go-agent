package agenttool_test

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dailz1/go-agent/agent"
	"github.com/dailz1/go-agent/agenttool"
	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

type settleGate struct {
	entered, cancelled, release chan struct{}
	stop                        func()
	testContext                 context.Context
	calls                       atomic.Int32
}

func newSettleGate(t *testing.T) *settleGate {
	t.Helper()
	g := &settleGate{
		entered: make(chan struct{}), cancelled: make(chan struct{}),
		release: make(chan struct{}), testContext: t.Context(),
	}
	g.stop = sync.OnceFunc(func() { close(g.release) })
	t.Cleanup(g.stop)
	return g
}

func (*settleGate) Info() tool.ToolInfo {
	return tool.ToolInfo{Name: "leaf", Parameters: tool.NewParameterSchema()}
}

func (g *settleGate) Execute(ctx context.Context, _ json.RawMessage) (*tool.ToolResult, error) {
	g.calls.Add(1)
	close(g.entered)
	<-ctx.Done()
	close(g.cancelled)
	select {
	case <-g.release:
	case <-g.testContext.Done():
	}
	return nil, ctx.Err()
}

func settleCall(id, name string) []llm.Chunk {
	return toolCallChunks([]llm.ToolUseBlock{{ID: id, Name: name, Input: json.RawMessage(`{"input":"child work"}`)}})
}

func settleText() []llm.Chunk {
	return []llm.Chunk{llm.TextDeltaChunk{Text: "finished"}, llm.DoneChunk{FinishReason: "stop"}}
}

type settleFixture struct {
	parent, child                 *agent.Agent
	parentStore, childStore       store.Store
	parentProvider, childProvider *roundContextSequenceProvider
	gate                          *settleGate
	parentExits, childExits       chan agent.SettlementToken
	childIDs                      chan string
	cancel                        context.CancelFunc
	outcome                       chan error
	joined                        chan struct{}
}

func newSettleFixture(t *testing.T, parentStore, childStore store.Store) *settleFixture {
	t.Helper()
	f := &settleFixture{
		parentStore: parentStore, childStore: childStore, gate: newSettleGate(t),
		parentExits: make(chan agent.SettlementToken, 8), childExits: make(chan agent.SettlementToken, 8),
		childIDs: make(chan string, 8), outcome: make(chan error, 1), joined: make(chan struct{}),
		parentProvider: &roundContextSequenceProvider{steps: [][]llm.Chunk{
			settleCall("parent-call", "child"), settleText(), settleText(),
		}},
		childProvider: &roundContextSequenceProvider{steps: [][]llm.Chunk{
			settleCall("leaf-call", "leaf"), settleText(), settleText(),
		}},
	}
	registry := tool.NewRegistry()
	registry.MustRegister(f.gate)
	options := []agent.Option{
		agent.WithCompactor(nil),
		agent.WithRunExitFn(func(token agent.SettlementToken) { f.childExits <- token }),
		agent.WithRoundContextProvider(func(_ context.Context, request agent.RoundContextRequest) (string, error) {
			f.childIDs <- request.ThreadID
			return "", nil
		}),
	}
	if childStore != nil {
		options = append(options, agent.WithStore(childStore))
	}
	f.child = agent.New(f.childProvider, registry, options...)
	parentRegistry := tool.NewRegistry()
	parentRegistry.MustRegister(agenttool.New(f.child, agenttool.Config{Name: "child"}))
	f.parent = agent.New(f.parentProvider, parentRegistry,
		agent.WithStore(parentStore), agent.WithCompactor(nil),
		agent.WithRunExitFn(func(token agent.SettlementToken) { f.parentExits <- token }),
	)
	ctx, cancel := context.WithCancel(t.Context())
	f.cancel = cancel
	go func() {
		defer close(f.joined)
		_, err := f.parent.RunThread(ctx, "parent", "original parent work")
		f.outcome <- err
	}()
	t.Cleanup(func() {
		cancel()
		f.gate.stop()
		settleJoin(t, f.joined)
	})
	await(t, f.gate.entered)
	return f
}

func settleJoin(t *testing.T, joined <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	select {
	case <-joined:
	case <-ctx.Done():
		t.Fatal("owned run did not exit")
	}
}

func (f *settleFixture) interrupt(t *testing.T) (agent.SettlementToken, agent.SettlementToken, error) {
	t.Helper()
	f.cancel()
	await(t, f.gate.cancelled)
	f.gate.stop()
	err := await(t, f.outcome)
	await(t, f.joined)
	parent := await(t, f.parentExits)
	var child agent.SettlementToken
	if f.childStore != nil {
		child = await(t, f.childExits)
	}
	return parent, child, err
}
