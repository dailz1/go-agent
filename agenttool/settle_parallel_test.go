package agenttool_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/dailz1/go-agent/agent"
	"github.com/dailz1/go-agent/agenttool"
	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

func TestSettleE59ParallelChildrenCollectedByCallbacks(t *testing.T) {
	for _, name := range []string{"independent stores", "shared store"} {
		t.Run(name, func(t *testing.T) {
			parentStore := store.NewMemory()
			type exit struct {
				index int
				token agent.SettlementToken
			}
			exits := make(chan exit, 2)
			parentExits := make(chan agent.SettlementToken, 1)
			registry := tool.NewRegistry()
			children := make([]*agent.Agent, 2)
			stores := make([]store.Store, 2)
			gates := make([]*settleGate, 2)
			providers := make([]*roundContextSequenceProvider, 2)
			for index, toolName := range []string{"first", "second"} {
				stores[index] = store.NewMemory()
				if name == "shared store" {
					stores[index] = parentStore
				}
				gates[index] = newSettleGate(t)
				childRegistry := tool.NewRegistry()
				childRegistry.MustRegister(gates[index])
				providers[index] = &roundContextSequenceProvider{steps: [][]llm.Chunk{settleCall("leaf-call", "leaf")}}
				children[index] = agent.New(providers[index], childRegistry, agent.WithStore(stores[index]),
					agent.WithRunExitFn(func(token agent.SettlementToken) { exits <- exit{index: index, token: token} }))
				registry.MustRegister(agenttool.New(children[index], agenttool.Config{Name: toolName}))
			}
			provider := &roundContextSequenceProvider{steps: [][]llm.Chunk{toolCallChunks([]llm.ToolUseBlock{
				{ID: "first-call", Name: "first", Input: json.RawMessage(`{"input":"one"}`)},
				{ID: "second-call", Name: "second", Input: json.RawMessage(`{"input":"two"}`)},
			})}}
			parent := agent.New(provider, registry, agent.WithStore(parentStore), agent.WithToolConcurrency(2),
				agent.WithRunExitFn(func(token agent.SettlementToken) { parentExits <- token }))
			ctx, cancel := context.WithCancel(t.Context())
			outcome, joined := make(chan error, 1), make(chan struct{})
			go func() {
				defer close(joined)
				_, err := parent.RunThread(ctx, "parent", "parallel delegation")
				outcome <- err
			}()
			t.Cleanup(func() {
				cancel()
				for _, gate := range gates {
					gate.stop()
				}
				settleJoin(t, joined)
			})
			for _, gate := range gates {
				await(t, gate.entered)
			}
			cancel()
			for _, gate := range gates {
				await(t, gate.cancelled)
				gate.stop()
			}
			err := await(t, outcome)
			await(t, joined)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("parallel error = %v", err)
			}
			parentToken := await(t, parentExits)
			owned := make([]settleOwnedTarget, 2)
			for range 2 {
				exit := await(t, exits)
				if owned[exit.index].agent != nil {
					t.Fatal("duplicate child exit callback")
				}
				owned[exit.index] = settleOwnedTarget{children[exit.index], exit.token}
			}
			if owned[0].token.ThreadID == owned[1].token.ThreadID {
				t.Fatal("parallel children share an identity")
			}
			// Only the first declaration's hard error reaches the parent chain.
			settleNestedTokens(t, err, parentToken, owned[0].token)
			h := settleHarness{parent: settleOwnedTarget{parent, parentToken}, children: owned, expectedChildren: 2}
			if _, err := h.cleanup(t.Context()); err != nil {
				t.Fatal(err)
			}
			for index, child := range owned {
				settleCancelled(t, child.agent, child.token, "leaf-call")
				settleOneCancellation(t, stores[index], child.token)
				if len(providers[index].requests) != 1 || gates[index].calls.Load() != 1 {
					t.Fatal("parallel child cleanup reexecuted work")
				}
			}
			settleCancelled(t, parent, parentToken, "first-call", "second-call")
			if len(provider.requests) != 1 || len(exits) != 0 || len(parentExits) != 0 {
				t.Fatal("cleanup generated execution or duplicate callbacks")
			}
		})
	}
}
