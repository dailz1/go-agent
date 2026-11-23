package agent

import (
	"context"
	"errors"
	"iter"
	"reflect"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/agenttest"
	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

type recoveryStreamWrapper struct {
	llm.Provider
	wrap func(iter.Seq2[llm.Chunk, error]) iter.Seq2[llm.Chunk, error]
}

func (p recoveryStreamWrapper) ChatStream(ctx context.Context, h []llm.Message, tools []tool.ToolInfo, opts ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	stream, err := p.Provider.ChatStream(ctx, h, tools, opts...)
	if err != nil {
		return nil, err
	}
	return p.wrap(stream), nil
}

func TestRecoveryStartedStreamDefersUntilResume(t *testing.T) {
	for _, tc := range []struct {
		name   string
		chunk  llm.Chunk
		paired bool
	}{
		{"text", &llm.TextDeltaChunk{Text: "partial"}, false},
		{"reasoning", &llm.ReasoningDeltaChunk{Text: "partial"}, false},
		{"tool", &llm.ToolCallStartChunk{ID: "unexecuted", Name: "missing"}, false},
		{"reasoning item", &llm.ReasoningItemChunk{}, false},
		{"usage", &llm.DoneChunk{Usage: &llm.Usage{InputTokens: 100}}, false},
		{"pointer", &llm.TextDeltaChunk{Text: "partial"}, false},
		{"typed nil", (*llm.TextDeltaChunk)(nil), true},
		{"wrapped", struct{ llm.Chunk }{&llm.TextDeltaChunk{Text: "wrapped"}}, true},
		{"same return", &llm.TextDeltaChunk{Text: "not separately yielded"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			history := []llm.Message{llm.UserMessage(strings.Repeat("x", 2000))}
			short := []llm.Message{llm.UserMessage("summary")}
			overflow := overflowTestError()
			first := recoveryExchange(history, nil, overflow, tc.chunk)
			if tc.paired {
				first.StreamChunks = nil
			}
			script := agenttest.NewScriptedProvider(first,
				recoveryExchange(short, nil, nil, &llm.TextDeltaChunk{Text: "done"}, &llm.DoneChunk{}))
			var provider llm.Provider = script
			if tc.paired {
				provider = recoveryStreamWrapper{Provider: script, wrap: func(stream iter.Seq2[llm.Chunk, error]) iter.Seq2[llm.Chunk, error] {
					return func(yield func(llm.Chunk, error) bool) {
						for chunk, err := range stream {
							if err != nil {
								chunk = tc.chunk
							}
							if !yield(chunk, err) {
								return
							}
						}
					}
				}}
			}
			compactions := 0
			a := New(provider, tool.NewRegistry(), WithStore(store.NewMemory()), WithLogger(discardLogger()),
				WithCompactor(CompactFunc(func(context.Context, []llm.Message, CompactionBudget, llm.Provider, []llm.Option) (CompactionResult, error) {
					compactions++
					return CompactionResult{History: short, Changed: true}, nil
				})))
			_, err := a.Run(t.Context(), messageString(history[0]), WithThread("recovery"))
			if !errors.Is(err, overflow) || compactions != 0 {
				t.Fatalf("started stream recovered in-call: error %v, compactions %d", err, compactions)
			}
			v := recoveryView(t, a)
			if v.recovery == nil || v.recovery.Phase != "pending" || v.recovery.Attempt != 0 ||
				!reflect.DeepEqual(v.history, history) {
				t.Fatalf("partial reply entered history or pending lost: %+v", v)
			}
			result, err := a.ResumeThread(t.Context(), "recovery")
			if err != nil || compactions != 1 || messageString(result.Message) != "done" {
				t.Fatalf("Resume = %+v, %v, compactions %d", result, err, compactions)
			}
			if err := script.Verify(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

type recoveryPermitProvider struct {
	llm.Provider
	held   bool
	exited func()
}

func (p *recoveryPermitProvider) ChatStream(ctx context.Context, h []llm.Message, tools []tool.ToolInfo, opts ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	if p.held {
		return nil, errors.New("stream permit still held")
	}
	p.held = true
	stream, err := p.Provider.ChatStream(ctx, h, tools, opts...)
	if err != nil {
		p.held = false
		return nil, err
	}
	return func(yield func(llm.Chunk, error) bool) {
		defer func() {
			p.held = false
			if p.exited != nil {
				p.exited()
			}
		}()
		for chunk, err := range stream {
			if !yield(chunk, err) {
				return
			}
		}
	}, nil
}

func (p *recoveryPermitProvider) Chat(ctx context.Context, h []llm.Message, tools []tool.ToolInfo, opts ...llm.Option) (*llm.Message, *llm.Usage, error) {
	if p.held {
		return nil, nil, errors.New("summary permit still held")
	}
	p.held = true
	defer func() { p.held = false }()
	return p.Provider.Chat(ctx, h, tools, opts...)
}

func TestRecoveryIteratorCleanupPrecedesCostlyActions(t *testing.T) {
	for _, cancelOnExit := range []bool{false, true} {
		t.Run(map[bool]string{false: "recover", true: "cancel during cleanup"}[cancelOnExit], func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			history := []llm.Message{llm.UserMessage(strings.Repeat("x", 2000))}
			short := []llm.Message{llm.UserMessage("summary")}
			exchanges := []agenttest.Exchange{recoveryExchange(history, nil, overflowTestError())}
			if !cancelOnExit {
				exchanges = append(exchanges, agenttest.Exchange{
					Method:       agenttest.MethodChat,
					Request:      agenttest.Request{Messages: short, Tools: []tool.ToolInfo{}, Options: llm.ApplyOptions(nil)},
					ChatResponse: &llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{&llm.TextBlock{Text: "summary"}}},
				}, recoveryExchange(short, nil, nil, &llm.TextDeltaChunk{Text: "done"}))
			}
			script := agenttest.NewScriptedProvider(exchanges...)
			provider := &recoveryPermitProvider{Provider: script}
			st := store.NewMemory()
			exits, summaries := 0, 0
			provider.exited = func() {
				exits++
				if exits == 1 {
					records, err := st.History(t.Context(), "recovery", 0)
					if err != nil {
						t.Fatal(err)
					}
					for _, record := range records {
						if record.Kind == store.KindContextRecovery {
							t.Fatal("recovery marker preceded iterator wrapper cleanup")
						}
					}
				}
				if cancelOnExit {
					cancel()
				}
			}
			a := New(provider, tool.NewRegistry(), WithStore(st), WithLogger(discardLogger()),
				WithCompactor(CompactFunc(func(ctx context.Context, _ []llm.Message, _ CompactionBudget, p llm.Provider, opts []llm.Option) (CompactionResult, error) {
					summaries++
					if exits != 1 || provider.held {
						t.Fatal("compactor entered before iterator wrapper released")
					}
					_, _, err := p.Chat(ctx, short, []tool.ToolInfo{}, opts...)
					return CompactionResult{History: short, Changed: true}, err
				})))
			_, err := a.Run(ctx, messageString(history[0]), WithThread("recovery"))
			if cancelOnExit {
				if !errors.Is(err, context.Canceled) || errors.Is(err, llm.ErrContextOverflow) || summaries != 0 {
					t.Fatalf("cancel lost precedence: %v, summaries %d", err, summaries)
				}
				if recoveryView(t, a).recovery != nil {
					t.Fatal("marker written after cleanup cancelled context")
				}
			} else if err != nil || summaries != 1 || exits != 2 {
				t.Fatalf("recovery = %v, summaries %d, exits %d", err, summaries, exits)
			}
			if err := script.Verify(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRecoveryEphemeralStartedReturnsOriginalError(t *testing.T) {
	history := []llm.Message{llm.UserMessage("input")}
	overflow := overflowTestError()
	p := agenttest.NewScriptedProvider(recoveryExchange(history, nil, overflow, &llm.TextDeltaChunk{Text: "partial"}))
	a := New(p, tool.NewRegistry(), WithLogger(discardLogger()), WithCompactor(recoveryCompactor()))
	_, err := a.Run(t.Context(), "input")
	if !errors.Is(err, overflow) || errors.Is(err, ErrContextRecoveryFailed) {
		t.Fatalf("started ephemeral stream error = %v", err)
	}
	if err := p.Verify(); err != nil {
		t.Fatal(err)
	}
}
