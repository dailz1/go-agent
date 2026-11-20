package agent

import (
	"context"
	"encoding/json"
	"iter"
	"reflect"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/agenttest"
	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

type recoveryNamedProvider struct {
	llm.Provider
	name string
}

func (p recoveryNamedProvider) Name() string { return p.name }

type recoveryDynamicOptionProvider struct {
	llm.Provider
	model   *string
	windows []string
}

func (p *recoveryDynamicOptionProvider) ContextWindow(model string) (int, bool) {
	p.windows = append(p.windows, model)
	return 4000, true
}

func (p *recoveryDynamicOptionProvider) ChatStream(ctx context.Context, messages []llm.Message, tools []tool.ToolInfo, opts ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	seq, err := p.Provider.ChatStream(ctx, messages, tools, opts...)
	*p.model = "B"
	return seq, err
}

func TestRecoveryDynamicOptionsFreezeAtRoundEntrance(t *testing.T) {
	original := []llm.Message{llm.UserMessage(strings.Repeat("x", 2000))}
	short := []llm.Message{llm.UserMessage("summary")}
	first := recoveryExchange(original, overflowTestError(), nil)
	second := recoveryExchange(short, nil, nil, llm.TextDeltaChunk{Text: "done"})
	first.Request.Options.Model, second.Request.Options.Model = "A", "A"
	script := agenttest.NewScriptedProvider(first, second)
	model, evaluations := "A", 0
	p := &recoveryDynamicOptionProvider{Provider: script, model: &model}
	st := store.NewMemory()
	a := New(p, tool.NewRegistry(), WithStore(st), WithLogger(discardLogger()),
		WithLLMOptions(func(o *llm.Options) { evaluations++; o.Model = model }),
		WithCompactor(CompactFunc(func(ctx context.Context, _ []llm.Message, b CompactionBudget, _ llm.Provider, opts []llm.Option) (CompactionResult, error) {
			if llm.ApplyOptions(opts).Model != "A" || b.MaxRunes != recoveryBudget(4000, 3200, estimateRunes(original), 0) {
				t.Fatal("compactor changed frozen request identity or window")
			}
			records, err := st.History(ctx, "recovery", 0)
			if err != nil {
				t.Fatal(err)
			}
			pending, err := decodeContextRecovery(records[1])
			if err != nil || pending.Model != "A" {
				t.Fatalf("pending identity differs from rejected request: %+v, %v", pending, err)
			}
			return CompactionResult{History: short, Changed: true}, nil
		})))
	if _, err := a.Run(t.Context(), messageString(original[0]), WithThread("recovery")); err != nil {
		t.Fatal(err)
	}
	if evaluations != 1 || !reflect.DeepEqual(p.windows, []string{"A"}) {
		t.Fatalf("options reevaluated within round: %d, windows %v", evaluations, p.windows)
	}
	if err := script.Verify(); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryBlockedConfigurationRequiresExplicitRepair(t *testing.T) {
	for _, mode := range []string{"same config", "thinking only", "round trip", "default drift", "explicit provider model"} {
		t.Run(mode, func(t *testing.T) {
			original := []llm.Message{llm.UserMessage(strings.Repeat("x", 2000))}
			short := []llm.Message{llm.UserMessage(strings.Repeat("s", 800))}
			first := recoveryExchange(original, overflowTestError(), nil)
			second := recoveryExchange(short, overflowTestError(), nil)
			first.Request.Options = llm.ApplyOptions([]llm.Option{llm.WithModel("A")})
			second.Request.Options = first.Request.Options
			script := agenttest.NewScriptedProvider(first, second)
			st := store.NewMemory()
			compactions := 0
			a := New(script, tool.NewRegistry(), WithStore(st), WithLogger(discardLogger()), WithLLMOptions(llm.WithModel("A")),
				WithCompactor(CompactFunc(func(context.Context, []llm.Message, CompactionBudget, llm.Provider, []llm.Option) (CompactionResult, error) {
					compactions++
					return CompactionResult{History: short, Changed: compactions == 1}, nil
				})))
			_, err := a.Run(t.Context(), messageString(original[0]), WithThread("recovery"))
			requireRecoveryError(t, err, "no_progress", 2)
			before := recoveryView(t, a)
			if err := script.Verify(); err != nil {
				t.Fatal(err)
			}
			options := []Option{WithStore(st), WithLogger(discardLogger()), WithCompactor(nil), WithLLMOptions(llm.WithModel("A"))}
			next := agenttest.NewScriptedProvider()
			switch mode {
			case "thinking only":
				if err := SetThreadConfig(t.Context(), st, "recovery", ConfigChange{Thinking: strptr("high")}); err != nil {
					t.Fatal(err)
				}
			case "round trip":
				for _, model := range []string{"B", "A"} {
					if err := SetThreadConfig(t.Context(), st, "recovery", ConfigChange{Model: &model}); err != nil {
						t.Fatal(err)
					}
				}
			case "default drift":
				options = append(options, WithLLMOptions(llm.WithModel("B")))
			case "explicit provider model":
				if err := SetThreadConfig(t.Context(), st, "recovery", ConfigChange{Provider: strptr("new"), Model: strptr("B")}); err != nil {
					t.Fatal(err)
				}
				if err := SetThreadConfig(t.Context(), st, "recovery", ConfigChange{Thinking: strptr("high")}); err != nil {
					t.Fatal(err)
				}
				success := recoveryExchange(short, nil, nil, llm.TextDeltaChunk{Text: "done"})
				success.Request.Options = llm.ApplyOptions([]llm.Option{llm.WithModel("B"), llm.WithThinking("high")})
				next = agenttest.NewScriptedProvider(success)
				options = append(options, WithProviders(recoveryNamedProvider{Provider: next, name: "new"}))
			}
			a = New(next, tool.NewRegistry(), options...)
			result, err := a.ResumeThread(t.Context(), "recovery")
			switch mode {
			case "explicit provider model":
				if err != nil || messageString(result.History[0]) != messageString(short[0]) {
					t.Fatalf("explicit repair lost compressed history: %+v, %v", result, err)
				}
				v := recoveryView(t, a)
				if v.recovery != nil || v.runActive {
					t.Fatalf("repaired run remains active: %+v", v)
				}
				records, err := st.History(t.Context(), "recovery", 0)
				if err != nil {
					t.Fatal(err)
				}
				var superseded *contextRecoveryPayload
				for _, record := range records {
					if record.Kind != store.KindContextRecovery {
						continue
					}
					payload, err := decodeContextRecovery(record)
					if err != nil {
						t.Fatal(err)
					}
					if payload.Phase == "superseded" {
						superseded = &payload
					}
				}
				if superseded == nil || *superseded.ModelChangeSeq >= *superseded.SelectedConfigSeq || superseded.Model != "A" {
					t.Fatalf("frozen old/new config proof lost: %+v", superseded)
				}
			default:
				reason := "no_progress"
				if mode == "default drift" {
					reason = "config_mismatch"
				}
				requireRecoveryError(t, err, reason, 2)
				v := recoveryView(t, a)
				if !reflect.DeepEqual(v.recovery, before.recovery) || !reflect.DeepEqual(v.history, short) {
					t.Fatalf("configuration incorrectly reset episode: %+v", v)
				}
			}
			if err := next.Verify(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRecoveryFreezesRoundAndDefersSteering(t *testing.T) {
	original := []llm.Message{llm.UserMessage(strings.Repeat("x", 2000))}
	short := []llm.Message{llm.UserMessage("summary")}
	nextHistory := append(deepCopyMessages(short), llm.AssistantMessage("continue"), llm.UserMessage("queued"))
	first := recoveryExchange(original, overflowTestError(), nil)
	retry := recoveryExchange(short, nil, nil, llm.TextDeltaChunk{Text: "continue"})
	success := recoveryExchange(nextHistory, nil, nil, llm.TextDeltaChunk{Text: "done"})
	first.Request.Options = llm.ApplyOptions([]llm.Option{llm.WithModel("A")})
	retry.Request.Options = first.Request.Options
	success.Request.Options = llm.ApplyOptions([]llm.Option{llm.WithModel("B")})
	late := mutatorTool("late", func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult("unused"), nil
	})
	nextRegistry := tool.NewRegistry()
	nextRegistry.MustRegister(late)
	success.Request.Tools = nextRegistry.List()
	p := agenttest.NewScriptedProvider(first, retry, success)
	st := store.NewMemory()
	var requests []RoundContextRequest
	var a *Agent
	hooks := 0
	a = New(p, tool.NewRegistry(), WithStore(st), WithLogger(discardLogger()), WithLLMOptions(llm.WithModel("A")),
		WithMaxIter(2), WithDynamicTools(),
		WithRoundContextProvider(func(_ context.Context, request RoundContextRequest) (string, error) {
			requests = append(requests, request)
			return "", nil
		}),
		WithCompactor(CompactFunc(func(ctx context.Context, _ []llm.Message, _ CompactionBudget, provider llm.Provider, opts []llm.Option) (CompactionResult, error) {
			if provider != p || llm.ApplyOptions(opts).Model != "A" {
				t.Fatal("compactor changed frozen round config")
			}
			if err := SetThreadConfig(ctx, st, "recovery", ConfigChange{Model: strptr("B")}); err != nil {
				t.Fatal(err)
			}
			if _, err := a.EnqueueInputWithID(ctx, "recovery", "q", "queued"); err != nil {
				t.Fatal(err)
			}
			if err := a.registry.Register(late); err != nil {
				t.Fatal(err)
			}
			return CompactionResult{History: short, Changed: true}, nil
		})),
		WithFinishRound(func(context.Context, FinishRoundContext) (FinishRoundDecision, error) {
			hooks++
			return FinishRoundDecision{Action: FinishRoundContinue}, nil
		}))
	result, err := a.Run(t.Context(), messageString(original[0]), WithThread("recovery"))
	if err != nil {
		t.Fatal(err)
	}
	want := []RoundContextRequest{{Round: 0, Attempt: 1, ThreadID: "recovery"}, {Round: 0, Attempt: 2, ThreadID: "recovery"}, {Round: 1, Attempt: 1, ThreadID: "recovery"}}
	if !reflect.DeepEqual(requests, want) || hooks != 1 || result.Truncated {
		t.Fatalf("round integration: requests %+v, hooks %d, result %+v", requests, hooks, result)
	}
	if err := p.Verify(); err != nil {
		t.Fatal(err)
	}
	v := recoveryView(t, a)
	if len(v.queue) != 0 || v.recovery != nil {
		t.Fatalf("steering or live recovery not settled: %+v", v)
	}
}

func TestRecoveryDisabledPreservesMarkerAfterExplicitModelChange(t *testing.T) {
	original := []llm.Message{llm.UserMessage(strings.Repeat("x", 2000))}
	p := agenttest.NewScriptedProvider(recoveryExchange(original, overflowTestError(), nil))
	st := store.NewMemory()
	a := New(p, tool.NewRegistry(), WithStore(st), WithLogger(discardLogger()), WithOverflowRecovery(false))
	_, err := a.Run(t.Context(), messageString(original[0]), WithThread("recovery"))
	requireRecoveryError(t, err, "disabled", 0)
	if err := SetThreadConfig(t.Context(), st, "recovery", ConfigChange{
		Provider: strptr("not-injected"), Model: strptr("new-model"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.EnqueueInputWithID(t.Context(), "recovery", "queued", "pending input"); err != nil {
		t.Fatal(err)
	}
	before := recoveryView(t, a)
	_, err = a.ResumeThread(t.Context(), "recovery")
	requireRecoveryError(t, err, "disabled", 0)
	after := recoveryView(t, a)
	if after.head != before.head || !reflect.DeepEqual(after.recovery, before.recovery) ||
		!reflect.DeepEqual(after.queue, before.queue) || !reflect.DeepEqual(after.history, before.history) {
		t.Fatalf("disabled Resume changed saved state: before %+v, after %+v", before, after)
	}
	if err := p.Verify(); err != nil {
		t.Fatal(err)
	}
}
