package agent

import (
	"context"
	"errors"
	"iter"
	"reflect"
	"testing"

	"github.com/dailz1/go-agent/pkg/llm"
	"github.com/dailz1/go-agent/pkg/store"
	"github.com/dailz1/go-agent/pkg/tool"
)

func terminalStoreAgent(t *testing.T, st store.Store) *Agent {
	t.Helper()
	registry := tool.NewRegistry()
	registry.MustRegister(&mockTool{info: tool.ToolInfo{Name: "echo"}, result: tool.NewTextResult("must not run")})
	return New(
		NewMockProvider(MsgResponse(toolCallMessage("c1", "c2"))),
		registry,
		WithStore(st),
		WithMaxIter(1),
		WithLogger(discardLogger()),
	)
}

func TestTerminalMaxIterStoreBarrierAndClosure(t *testing.T) {
	st := store.NewMemory()
	seq, err := terminalStoreAgent(t, st).RunThreadStream(context.Background(), "t", "go")
	if err != nil {
		t.Fatalf("RunThreadStream: %v", err)
	}
	var events []AgentEvent
	for event, streamErr := range seq {
		if streamErr != nil {
			t.Fatalf("stream: %v", streamErr)
		}
		events = append(events, event)
		switch event.(type) {
		case ToolCallEvent, ToolResultEvent:
			equalKinds(t, logKinds(t, st, "t"),
				store.KindRunStarted, store.KindRoundDeclared, store.KindRoundCommitted, store.KindAgentEvent)
		}
	}
	if got := terminalDone(t, events); !got.Truncated || got.ThreadID != "t" {
		t.Errorf("Done = %#v, want closed truncated thread", got)
	}
}

func TestTerminalMaxIterConsumerBreakAndCancellationStillClose(t *testing.T) {
	for _, test := range []struct {
		name       string
		persistent bool
		atResult   bool
		cancel     bool
	}{
		{name: "non-store first declaration"},
		{name: "non-store first result", atResult: true},
		{name: "store first declaration", persistent: true},
		{name: "store first result", persistent: true, atResult: true},
		{name: "non-store cancellation after declaration", cancel: true},
		{name: "non-store cancellation after result", atResult: true, cancel: true},
		{name: "store cancellation after declaration", persistent: true, cancel: true},
		{name: "store cancellation after result", persistent: true, atResult: true, cancel: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var agent *Agent
			var st store.Store
			if test.persistent {
				st = store.NewMemory()
				agent = terminalStoreAgent(t, st)
			} else {
				registry := tool.NewRegistry()
				registry.MustRegister(&mockTool{info: tool.ToolInfo{Name: "echo"}})
				agent = New(
					NewMockProvider(MsgResponse(toolCallMessage("c1", "c2"))),
					registry, WithMaxIter(1), WithLogger(discardLogger()),
				)
			}
			var seq iter.Seq2[AgentEvent, error]
			var err error
			if test.persistent {
				seq, err = agent.RunThreadStream(ctx, "t", "go")
			} else {
				seq, err = agent.RunStream(ctx, "go")
			}
			if err != nil {
				t.Fatalf("RunStream: %v", err)
			}
			var events []AgentEvent
			for event, streamErr := range seq {
				if streamErr != nil {
					t.Fatalf("stream: %v", streamErr)
				}
				events = append(events, event)
				_, isResult := event.(ToolResultEvent)
				_, isCall := event.(ToolCallEvent)
				if (test.atResult && isResult) || (!test.atResult && isCall) {
					if test.cancel {
						cancel()
					} else {
						break
					}
				}
			}
			if test.cancel {
				if len(events) != 5 {
					t.Fatalf("events after cancellation = %d, want all terminal events", len(events))
				}
			}
			if test.persistent {
				if !hasKind(t, st, "t", store.KindRoundCommitted) || !hasKind(t, st, "t", store.KindAgentEvent) {
					t.Fatalf("store did not preclose terminal batch: %v", logKinds(t, st, "t"))
				}
				if _, err := agent.ResumeThread(context.Background(), "t"); !errors.Is(err, ErrNothingToResume) {
					t.Errorf("ResumeThread = %v, want ErrNothingToResume", err)
				}
			}
		})
	}
}

func TestTerminalMaxIterPersistenceFailureRecovery(t *testing.T) {
	for _, test := range []struct {
		name       string
		failOnCall int
		wantKinds  []string
		wantEvents int
	}{
		{
			name:       "declaration failure leaves active preterminal run",
			failOnCall: 2, // started, then declared
			wantKinds:  []string{store.KindRunStarted},
		},
		{
			name:       "commit failure leaves declared round for unknown recovery",
			failOnCall: 3, // started, declared, then committed
			wantKinds:  []string{store.KindRunStarted, store.KindRoundDeclared},
		},
		{
			name:       "done failure leaves committed round for completion recovery",
			failOnCall: 4, // started, declared, committed, then done
			wantKinds:  []string{store.KindRunStarted, store.KindRoundDeclared, store.KindRoundCommitted},
			wantEvents: 0,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			st := &failAppendStore{Store: store.NewMemory(), failOnCall: test.failOnCall}
			agent := terminalStoreAgent(t, st)
			agent.provider = NewMockProvider(
				MsgResponse(toolCallMessage("c1", "c2")),
				MsgResponse(llm.AssistantMessage("recovered")),
			)
			seq, err := agent.RunThreadStream(context.Background(), "t", "go")
			if err != nil {
				t.Fatalf("RunThreadStream: %v", err)
			}
			var events []AgentEvent
			var terminal error
			for event, streamErr := range seq {
				if streamErr != nil {
					terminal = streamErr
					break
				}
				events = append(events, event)
			}
			if terminal == nil || !reflect.DeepEqual(logKinds(t, st, "t"), test.wantKinds) {
				t.Fatalf("terminal=%v log=%v, want failure and %v", terminal, logKinds(t, st, "t"), test.wantKinds)
			}
			if len(events) != test.wantEvents {
				t.Errorf("events before persistence error = %d, want %d", len(events), test.wantEvents)
			}
			if _, err := agent.ResumeThread(context.Background(), "t"); err != nil {
				t.Fatalf("ResumeThread recovery: %v", err)
			}
		})
	}
}

type cancelAfterTerminalProvider struct {
	cancel context.CancelFunc
}

func (p *cancelAfterTerminalProvider) Name() string { return "cancel-after-terminal" }

func (p *cancelAfterTerminalProvider) Chat(context.Context, []llm.Message, []tool.ToolInfo, ...llm.Option) (*llm.Message, *llm.Usage, error) {
	return nil, nil, errors.New("unexpected Chat call")
}

func (p *cancelAfterTerminalProvider) ChatStream(context.Context, []llm.Message, []tool.ToolInfo, ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	return func(yield func(llm.Chunk, error) bool) {
		if !yield(llm.ToolCallStartChunk{Index: 0, ID: "c1", Name: "echo"}, nil) ||
			!yield(llm.ToolCallArgsChunk{Index: 0, Delta: `{}`}, nil) ||
			!yield(llm.DoneChunk{FinishReason: "tool_calls"}, nil) {
			return
		}
		p.cancel()
	}, nil
}

func TestTerminalMaxIterPrecommitCancellationDoesNotDeclare(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st := store.NewMemory()
	registry := tool.NewRegistry()
	registry.MustRegister(&mockTool{info: tool.ToolInfo{Name: "echo"}})
	agent := New(
		&cancelAfterTerminalProvider{cancel: cancel},
		registry,
		WithStore(st),
		WithMaxIter(1),
		WithLogger(discardLogger()),
	)
	seq, err := agent.RunThreadStream(ctx, "t", "go")
	if err != nil {
		t.Fatalf("RunThreadStream: %v", err)
	}
	events, errs := collectEvents(t, seq, nil)
	if len(events) != 0 || len(errs) != 1 || !errors.Is(errs[0], context.Canceled) {
		t.Fatalf("events=%v errs=%v, want only context.Canceled", events, errs)
	}
	equalKinds(t, logKinds(t, st, "t"), store.KindRunStarted)

	if _, err := agent.ResumeThread(context.Background(), "t"); err != nil {
		t.Fatalf("ResumeThread after precommit cancellation: %v", err)
	}
	equalKinds(t, logKinds(t, st, "t"),
		store.KindRunStarted, store.KindRoundDeclared, store.KindRoundCommitted, store.KindAgentEvent)
}

func TestTerminalMaxIterStoreAndNonStoreResultsMatch(t *testing.T) {
	registry := tool.NewRegistry()
	registry.MustRegister(&mockTool{info: tool.ToolInfo{Name: "echo"}})
	nonPersistentSeq, err := New(
		NewMockProvider(MsgResponse(toolCallMessage("c1", "c2"))),
		registry,
		WithMaxIter(1),
		WithLogger(discardLogger()),
	).RunStream(context.Background(), "go")
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	nonPersistentEvents, nonPersistentErrs := collectEvents(t, nonPersistentSeq, nil)
	if len(nonPersistentErrs) != 0 {
		t.Fatalf("non-Store RunStream errors = %v", nonPersistentErrs)
	}

	st := store.NewMemory()
	persistentSeq, err := terminalStoreAgent(t, st).RunThreadStream(context.Background(), "t", "go")
	if err != nil {
		t.Fatalf("RunThreadStream: %v", err)
	}
	persistentEvents, persistentErrs := collectEvents(t, persistentSeq, nil)
	if len(persistentErrs) != 0 {
		t.Fatalf("Store RunThreadStream errors = %v", persistentErrs)
	}

	nonPersistentDone := terminalDone(t, nonPersistentEvents)
	persistentDone := terminalDone(t, persistentEvents)
	if !reflect.DeepEqual(nonPersistentDone.History, persistentDone.History) {
		t.Errorf("terminal history diverged:\nnon-store=%#v\nstore=%#v", nonPersistentDone.History, persistentDone.History)
	}

	nonPersistentDone.ThreadID = ""
	persistentDone.ThreadID = ""
	if !reflect.DeepEqual(nonPersistentDone, persistentDone) {
		t.Errorf("DoneEvent diverged excluding ThreadID:\nnon-store=%#v\nstore=%#v", nonPersistentDone, persistentDone)
	}

	nonPersistentComparable := append([]AgentEvent(nil), nonPersistentEvents...)
	persistentComparable := append([]AgentEvent(nil), persistentEvents...)
	nonPersistentComparable[len(nonPersistentComparable)-1] = nonPersistentDone
	persistentComparable[len(persistentComparable)-1] = persistentDone
	if !reflect.DeepEqual(nonPersistentComparable, persistentComparable) {
		t.Errorf("value event sequence diverged excluding DoneEvent.ThreadID:\nnon-store=%#v\nstore=%#v", nonPersistentComparable, persistentComparable)
	}
}
