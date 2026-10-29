package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

func TestFinishRoundEndAfterToolRound(t *testing.T) {
	st := store.NewMemory()
	reg := tool.NewRegistry()
	reg.MustRegister(mutatorTool("calc", func(ctx context.Context, _ json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult("42"), nil
	}))
	provider := NewMockStreamingProvider([][]llm.Chunk{
		toolCallChunks("c1", "calc"),
		textChunks("unused"),
	})
	calls := 0
	agent := New(provider, reg,
		WithStore(st),
		WithLogger(discardLogger()),
		WithFinishRound(func(ctx context.Context, fc FinishRoundContext) (FinishRoundDecision, error) {
			calls++
			return FinishRoundDecision{Action: FinishRoundEnd}, nil
		}),
	)
	res, err := agent.Run(context.Background(), "compute", WithThread("t"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if calls != 1 {
		t.Fatalf("hook calls = %d, want 1", calls)
	}
	if res.Truncated {
		t.Fatal("End must not be truncated")
	}
	blocks := historyResultBlocks(res.History)
	if len(blocks) != 1 {
		t.Fatalf("history result blocks = %d, want 1 (ends with tool results)", len(blocks))
	}
	if res.Message.Role != llm.RoleAssistant {
		t.Fatalf("Done message role = %v, want the round assistant", res.Message.Role)
	}
	state, err := st.Latest(context.Background(), "t")
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	for _, r := range state.Tail {
		if r.Kind == store.KindRunClosed {
			closed = true
		}
		if r.Kind == store.KindRunCancelled {
			t.Fatal("cooperative End must not write run_cancelled")
		}
	}
	if !closed {
		t.Fatal("run_closed record missing")
	}
	if _, err := agent.ResumeThread(context.Background(), "t"); err != ErrNothingToResume {
		t.Fatalf("Resume after End = %v, want ErrNothingToResume", err)
	}
}

func TestFinishRoundContinueKeepsNoToolRunAlive(t *testing.T) {
	st := store.NewMemory()
	provider := NewMockStreamingProvider([][]llm.Chunk{
		textChunks("first reply"),
		textChunks("second reply"),
	})
	agent := New(provider, tool.NewRegistry(),
		WithStore(st),
		WithLogger(discardLogger()),
		WithFinishRound(func(ctx context.Context, fc FinishRoundContext) (FinishRoundDecision, error) {
			if fc.Round == 0 {
				return FinishRoundDecision{Action: FinishRoundContinue}, nil
			}
			return FinishRoundDecision{}, nil
		}),
	)
	res, err := agent.Run(context.Background(), "hello", WithThread("t"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(provider.AllMessages) != 2 {
		t.Fatalf("provider calls = %d, want 2 (Continue keeps the run alive)", len(provider.AllMessages))
	}
	second := provider.AllMessages[1]
	last := second[len(second)-1]
	if last.Role != llm.RoleAssistant || !strings.Contains(messageString(last), "first reply") {
		t.Fatalf("round-2 outbound tail = %v/%v, want the declared assistant turn re-sent", last.Role, messageString(last))
	}
	turns := 0
	for _, m := range res.History {
		if m.Role == llm.RoleAssistant {
			turns++
		}
	}
	if turns != 2 {
		t.Fatalf("assistant turns in history = %d, want 2", turns)
	}
}

func TestFinishRoundEndOnNoToolRound(t *testing.T) {
	st := store.NewMemory()
	provider := NewMockStreamingProvider([][]llm.Chunk{textChunks("the answer")})
	agent := New(provider, tool.NewRegistry(),
		WithStore(st),
		WithLogger(discardLogger()),
		WithFinishRound(func(ctx context.Context, fc FinishRoundContext) (FinishRoundDecision, error) {
			return FinishRoundDecision{Action: FinishRoundEnd}, nil
		}),
	)
	res, err := agent.Run(context.Background(), "hello", WithThread("t"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if messageString(res.Message) != "the answer" {
		t.Fatalf("Done message = %q, want the assistant turn", messageString(res.Message))
	}
	state, err := st.Latest(context.Background(), "t")
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	for _, r := range state.Tail {
		if r.Kind == store.KindRunClosed {
			closed = true
		}
	}
	if !closed {
		t.Fatal("run_closed record missing")
	}
}

func TestFinishRoundNotConsultedAtMaxIterBoundary(t *testing.T) {
	st := store.NewMemory()
	reg := tool.NewRegistry()
	reg.MustRegister(mutatorTool("calc", func(ctx context.Context, _ json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult("42"), nil
	}))
	provider := NewMockStreamingProvider([][]llm.Chunk{toolCallChunks("c1", "calc")})
	consulted := 0
	agent := New(provider, reg,
		WithStore(st),
		WithLogger(discardLogger()),
		WithMaxIter(1),
		WithFinishRound(func(ctx context.Context, fc FinishRoundContext) (FinishRoundDecision, error) {
			consulted++
			return FinishRoundDecision{Action: FinishRoundContinue}, nil
		}),
	)
	res, err := agent.Run(context.Background(), "compute", WithThread("t"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if consulted != 0 {
		t.Fatalf("hook consulted %d times at the maxIter boundary, want 0 (frozen)", consulted)
	}
	if !res.Truncated {
		t.Fatal("terminal skip must remain truncated")
	}
}

func TestFinishRoundErrorIsTypedAndResumable(t *testing.T) {
	st := store.NewMemory()
	reg := tool.NewRegistry()
	reg.MustRegister(mutatorTool("calc", func(ctx context.Context, _ json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult("42"), nil
	}))
	provider := NewMockStreamingProvider([][]llm.Chunk{
		toolCallChunks("c1", "calc"),
		toolCallChunks("c2", "calc"),
		textChunks("recovered"),
	})
	exploded := false
	var hookRounds []int
	agent := New(provider, reg,
		WithStore(st),
		WithLogger(discardLogger()),
		WithFinishRound(func(ctx context.Context, fc FinishRoundContext) (FinishRoundDecision, error) {
			hookRounds = append(hookRounds, fc.Round)
			if !exploded {
				exploded = true
				return FinishRoundDecision{}, errors.New("hook exploded")
			}
			// The resumed round's first response is a TOOL call: this hook
			// invocation is the tool-round call site (after commit) with
			// the resumed logical round — reverting the round offset makes
			// this assertion fail.
			if fc.Round == 1 {
				return FinishRoundDecision{Action: FinishRoundEnd}, nil
			}
			return FinishRoundDecision{}, nil
		}),
	)
	_, err := agent.Run(context.Background(), "compute", WithThread("t"))
	var frErr *FinishRoundError
	if !errors.As(err, &frErr) || !strings.Contains(err.Error(), "hook exploded") {
		t.Fatalf("err = %v, want FinishRoundError wrapping the cause", err)
	}
	res, rerr := agent.ResumeThread(context.Background(), "t")
	if rerr != nil {
		t.Fatalf("Resume after hook error: %v", rerr)
	}
	if res.Truncated || res.Message.Role != llm.RoleAssistant {
		t.Fatalf("resumed result unexpected: %+v", res)
	}
	// The resumed run's first round is logical round 1 (roundBase advances
	// past the durably declared round 0) — the old bug reported round 0.
	if len(hookRounds) != 2 || hookRounds[0] != 0 || hookRounds[1] != 1 {
		t.Fatalf("hook rounds = %v, want [0 1] (logical rounds across resume)", hookRounds)
	}
}

func TestFinishRoundDeclarationReplaysAcrossRestart(t *testing.T) {
	st := store.NewMemory()
	provider := NewMockStreamingProvider([][]llm.Chunk{
		textChunks("kept"),
		textChunks("settled"),
	})
	agent := New(provider, tool.NewRegistry(),
		WithStore(st),
		WithLogger(discardLogger()),
		WithFinishRound(func(ctx context.Context, fc FinishRoundContext) (FinishRoundDecision, error) {
			if fc.Round == 0 {
				return FinishRoundDecision{Action: FinishRoundContinue}, nil
			}
			return FinishRoundDecision{}, nil
		}),
	)
	if _, err := agent.Run(context.Background(), "hello", WithThread("t")); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(provider.AllMessages) != 2 {
		t.Fatalf("calls = %d", len(provider.AllMessages))
	}
	// A fresh agent over the same store must replay the terminal_text
	// declaration: the first run closed via run_closed; the fresh agent's
	// new run replays the terminal_text turn into history and the next
	// request carries the declared turn.
	provider2 := NewMockStreamingProvider([][]llm.Chunk{textChunks("again")})
	agent2 := New(provider2, tool.NewRegistry(), WithStore(st), WithLogger(discardLogger()))
	if _, err := agent2.Run(context.Background(), "next", WithThread("t")); err != nil {
		t.Fatalf("second run: %v", err)
	}
	joined := messageSliceString(provider2.AllMessages[0])
	if !strings.Contains(joined, "kept") {
		t.Fatalf("replayed outbound = %q, want the declared terminal_text turn", joined)
	}
}

func TestFinishRoundToolFreeContinueCrashWindow(t *testing.T) {
	st := store.NewMemory()
	agent := New(NewMockStreamingProvider([][]llm.Chunk{textChunks("first reply")}), tool.NewRegistry(),
		WithStore(st),
		WithLogger(discardLogger()),
		WithFinishRound(func(ctx context.Context, fc FinishRoundContext) (FinishRoundDecision, error) {
			state, err := st.Latest(ctx, fc.ThreadID)
			if err != nil {
				return FinishRoundDecision{}, err
			}
			declared := false
			for _, r := range state.Tail {
				if r.Kind != store.KindRoundDeclared {
					continue
				}
				var p roundDeclaredPayload
				if strictDecode(r.Payload, &p) == nil && p.Kind == terminalTextDeclaration {
					declared = true
				}
			}
			if !declared {
				return FinishRoundDecision{}, errors.New("terminal_text declaration not durable before hook")
			}
			return FinishRoundDecision{}, errors.New("interrupted after declaration")
		}),
	)
	_, err := agent.Run(context.Background(), "hello", WithThread("t"))
	var frErr *FinishRoundError
	if !errors.As(err, &frErr) || !strings.Contains(err.Error(), "interrupted after declaration") {
		t.Fatalf("err = %v, want FinishRoundError wrapping the interruption", err)
	}
	v, err := agent.replayThread(context.Background(), "t")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !v.runActive || v.open != nil {
		t.Fatalf("post-declaration state = active:%v open:%v, want active with no open declaration", v.runActive, v.open)
	}
	provider2 := NewMockStreamingProvider([][]llm.Chunk{textChunks("resumed done")})
	var resumedRounds []int
	agent2 := New(provider2, tool.NewRegistry(), WithStore(st), WithLogger(discardLogger()),
		WithFinishRound(func(ctx context.Context, fc FinishRoundContext) (FinishRoundDecision, error) {
			resumedRounds = append(resumedRounds, fc.Round)
			return FinishRoundDecision{}, nil
		}))
	res, err := agent2.ResumeThread(context.Background(), "t")
	if err != nil {
		t.Fatalf("ResumeThread: %v", err)
	}
	if got := messageSliceString(provider2.AllMessages[0]); strings.Count(got, "first reply") != 1 {
		t.Fatalf("resumed outbound = %q, want the declared turn exactly once", got)
	}
	turns := 0
	for _, m := range res.History {
		if m.Role == llm.RoleAssistant {
			turns++
		}
	}
	if turns != 2 {
		t.Fatalf("assistant turns after resume = %d, want 2 (declared turn + resumed reply)", turns)
	}
	if messageString(res.Message) != "resumed done" {
		t.Fatalf("resumed Done message = %q, want the resumed reply", messageString(res.Message))
	}
	if len(resumedRounds) != 1 || resumedRounds[0] != 1 {
		t.Fatalf("resumed hook rounds = %v, want [1] (logical round across resume, tool-free call site)", resumedRounds)
	}
	if res.Truncated {
		t.Fatal("resumed natural done must not be truncated")
	}
}

func TestFinishRoundInvalidActionRejected(t *testing.T) {
	agent := New(NewMockStreamingProvider([][]llm.Chunk{textChunks("done")}), tool.NewRegistry(),
		WithStore(store.NewMemory()),
		WithLogger(discardLogger()),
		WithFinishRound(func(ctx context.Context, fc FinishRoundContext) (FinishRoundDecision, error) {
			return FinishRoundDecision{Action: FinishRoundAction(99)}, nil
		}),
	)
	_, err := agent.Run(context.Background(), "hello", WithThread("t"))
	var frErr *FinishRoundError
	if !errors.As(err, &frErr) || !strings.Contains(err.Error(), "invalid finish round action") {
		t.Fatalf("err = %v, want FinishRoundError for the invalid action", err)
	}
}

// runClosedBoundaryStore injects one write-boundary failure exactly at the
// run_closed Append, mirroring settleCancelAfterAppend's one-shot gate:
// MemoryStore.Append rejects a canceled context on its first line, so a
// synchronous cancel before the delegate deterministically blocks the write.
type runClosedBoundaryStore struct {
	store.Store
	mode   string // "cancel_before_append" | "cancel_after_append" | "ack_lost"
	cancel context.CancelFunc
	fired  bool
}

func (s *runClosedBoundaryStore) Append(ctx context.Context, thread string, expected int64, records ...store.Record) (int64, error) {
	if s.fired || len(records) == 0 || records[0].Kind != store.KindRunClosed {
		return s.Store.Append(ctx, thread, expected, records...)
	}
	s.fired = true
	switch s.mode {
	case "cancel_before_append":
		s.cancel()
		return s.Store.Append(ctx, thread, expected, records...)
	case "cancel_after_append":
		head, err := s.Store.Append(ctx, thread, expected, records...)
		if err == nil {
			s.cancel()
		}
		return head, err
	default: // "ack_lost"
		if _, err := s.Store.Append(ctx, thread, expected, records...); err != nil {
			return 0, err
		}
		return 0, errors.New("ack lost")
	}
}

// TestRunClosedWriteBoundaries covers the three write states of the
// cooperative-end append: canceled before it lands, landed and acknowledged
// before a later cancel, and landed with the acknowledgement lost. The
// durable log, the exit token, and the resumability of the thread must match
// the state the log is actually in — never the state the caller believes in.
func TestRunClosedWriteBoundaries(t *testing.T) {
	for _, tc := range []struct{ name, mode string }{
		{"cancel_before_append", "cancel_before_append"},
		{"cancel_after_append_ack_ok", "cancel_after_append"},
		{"ack_lost", "ack_lost"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			st := &runClosedBoundaryStore{Store: store.NewMemory(), mode: tc.mode, cancel: cancel}
			reg := tool.NewRegistry()
			reg.MustRegister(mutatorTool("calc", func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
				return tool.NewTextResult("42"), nil
			}))
			provider := NewMockStreamingProvider([][]llm.Chunk{
				toolCallChunks("c1", "calc"),
				textChunks("recovered"),
			})
			var token SettlementToken
			agent := New(provider, reg,
				WithStore(st),
				WithLogger(discardLogger()),
				WithFinishRound(func(context.Context, FinishRoundContext) (FinishRoundDecision, error) {
					return FinishRoundDecision{Action: FinishRoundEnd}, nil
				}),
				WithRunExitFn(func(tk SettlementToken) { token = tk }),
			)
			res, err := agent.Run(ctx, "compute", WithThread("t"))
			switch tc.mode {
			case "cancel_before_append":
				var interrupted *RunInterruptedError
				if !errors.As(err, &interrupted) || !errors.Is(err, context.Canceled) {
					t.Fatalf("Run err = %v, want RunInterruptedError wrapping context.Canceled", err)
				}
				if res != nil {
					t.Fatal("a run whose run_closed never landed must not deliver a success Done")
				}
				equalKinds(t, logKinds(t, st, "t"),
					store.KindRunStarted, store.KindRoundDeclared, store.KindRoundCommitted)
				if token.ExpectedHead != 3 {
					t.Fatalf("exit token ExpectedHead = %d, want 3 (after the commit)", token.ExpectedHead)
				}
				v, rerr := agent.replayThread(context.Background(), "t")
				if rerr != nil || !v.runActive {
					t.Fatalf("replay after the boundary = active:%v err:%v, want an active run", v.runActive, rerr)
				}
				if _, err := agent.ResumeThread(context.Background(), "t"); err != nil {
					t.Fatalf("ResumeThread with a fresh context: %v", err)
				}
			case "cancel_after_append":
				if err != nil || res == nil || res.Truncated {
					t.Fatalf("Run = %v, %v: the durable terminal must not be undone by a later cancel", res, err)
				}
				equalKinds(t, logKinds(t, st, "t"),
					store.KindRunStarted, store.KindRoundDeclared, store.KindRoundCommitted, store.KindRunClosed)
				if _, err := agent.ResumeThread(context.Background(), "t"); err != ErrNothingToResume {
					t.Fatalf("Resume after the closed run = %v, want ErrNothingToResume", err)
				}
				if len(provider.AllMessages) != 1 {
					t.Fatalf("provider calls = %d, want 1 (no re-request after the closed run)", len(provider.AllMessages))
				}
			default: // "ack_lost"
				var interrupted *RunInterruptedError
				if !errors.As(err, &interrupted) || !strings.Contains(err.Error(), "ack lost") {
					t.Fatalf("Run err = %v, want RunInterruptedError wrapping the lost ack", err)
				}
				kinds := logKinds(t, st, "t")
				equalKinds(t, kinds,
					store.KindRunStarted, store.KindRoundDeclared, store.KindRoundCommitted, store.KindRunClosed)
				if token.ExpectedHead != 3 {
					t.Fatalf("exit token ExpectedHead = %d, want 3 (the session head never advanced past the commit)", token.ExpectedHead)
				}
				v, rerr := agent.replayThread(context.Background(), "t")
				if rerr != nil || v.runActive {
					t.Fatalf("replay = active:%v err:%v, want the run closed by the landed run_closed", v.runActive, rerr)
				}
				if _, err := agent.ResumeThread(context.Background(), "t"); err != ErrNothingToResume {
					t.Fatalf("Resume = %v, want ErrNothingToResume", err)
				}
				if len(provider.AllMessages) != 1 {
					t.Fatalf("provider calls = %d, want 1", len(provider.AllMessages))
				}
				if err := agent.SettleThread(context.Background(), token); !errors.Is(err, store.ErrRevisionConflict) {
					t.Fatalf("SettleThread with the stale token = %v, want store.ErrRevisionConflict", err)
				}
				// The failed settle wrote nothing: no compensating cancel, no reopen.
				equalKinds(t, logKinds(t, st, "t"), kinds...)
			}
		})
	}
}
