package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

func runOptionToolRounds(n int) [][]llm.Chunk {
	rounds := make([][]llm.Chunk, n)
	for i := range rounds {
		rounds[i] = toolCallChunks(fmt.Sprintf("c%d", i), "echo")
	}
	return rounds
}

// Both public entrypoints must consume the invocation's budget.
func runOptionInvoke(t *testing.T, a *Agent, stream bool, opts ...RunOption) *RunResult {
	t.Helper()
	if !stream {
		result, err := a.Run(t.Context(), "go", opts...)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	seq, err := a.RunStream(t.Context(), "go", opts...)
	events, errs := collectEvents(t, seq, err)
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	done := terminalDone(t, events)
	return &RunResult{Truncated: done.Truncated, ToolCalls: done.ToolCalls, History: done.History, ThreadID: done.ThreadID}
}

func TestRunOptionsBudget(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct {
			name string
			base []Option
			opts []RunOption
			want int
		}{
			{name: "smaller", base: []Option{WithMaxIter(10)}, opts: []RunOption{RunWithMaxIter(3)}, want: 3},
			{name: "larger", base: []Option{WithMaxIter(10)}, opts: []RunOption{RunWithMaxIter(30)}, want: 30},
			{name: "inherit", base: []Option{WithMaxIter(30)}, want: 30},
			{name: "last invalid", base: []Option{WithMaxIter(30)}, opts: []RunOption{RunWithMaxIter(3), RunWithMaxIter(0)}, want: 25},
			{name: "last valid", base: []Option{WithMaxIter(30)}, opts: []RunOption{RunWithMaxIter(0), RunWithMaxIter(3)}, want: 3},
			{name: "negative run", base: []Option{WithMaxIter(30)}, opts: []RunOption{RunWithMaxIter(-2)}, want: 25},
			{name: "system default", want: 25},
			{name: "zero constructor", base: []Option{WithMaxIter(0)}, want: 25},
			{name: "negative constructor", base: []Option{WithMaxIter(-2)}, want: 25},
			{name: "zero run", opts: []RunOption{RunWithMaxIter(0)}, want: 25},
		} {
			t.Run(fmt.Sprintf("%s/stream=%v", tc.name, stream), func(t *testing.T) {
				executed := 0
				reg := tool.NewRegistry()
				reg.MustRegister(mutatorTool("echo", func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
					executed++
					return tool.NewTextResult("ok"), nil
				}))
				provider := NewMockStreamingProvider(append(runOptionToolRounds(tc.want), runOptionToolRounds(30)...))
				a := New(provider, reg, append(tc.base, WithLogger(discardLogger()))...)
				result := runOptionInvoke(t, a, stream, tc.opts...)
				if provider.index != tc.want || executed != tc.want-1 || !result.Truncated || result.ToolCalls != tc.want {
					t.Fatalf("requests/executions/truncated/calls=%d/%d/%v/%d, want %d/%d/true/%d",
						provider.index, executed, result.Truncated, result.ToolCalls, tc.want, tc.want-1, tc.want)
				}
				results := historyResultBlocks(result.History)
				if len(results) != tc.want || !results[len(results)-1].IsError ||
					!strings.Contains(results[len(results)-1].Content, "iteration limit") {
					t.Fatalf("terminal skip missing: %+v", results)
				}
				// A later invocation inherits the original constructor policy.
				before := provider.index
				next := runOptionInvoke(t, a, stream)
				wantNext := 25
				if len(tc.base) != 0 && tc.name != "zero constructor" && tc.name != "negative constructor" {
					wantNext = 30
					if tc.name == "smaller" || tc.name == "larger" {
						wantNext = 10
					}
				}
				if provider.index-before != wantNext || next.ToolCalls != wantNext {
					t.Fatalf("later default requests/calls=%d/%d, want %d", provider.index-before, next.ToolCalls, wantNext)
				}
			})
		}
	}
}

func TestRunOptionsDurableBudgetAndTerminalBreak(t *testing.T) {
	for _, id := range []string{"named", ""} {
		for _, stop := range []bool{false, true} {
			t.Run(fmt.Sprintf("id=%q/break=%v", id, stop), func(t *testing.T) {
				st := store.NewMemory()
				a := terminalStoreAgent(t, st)
				a.maxIter = 30
				var thread string
				a.runExitFn = func(token SettlementToken) { thread = token.ThreadID }
				if stop {
					seq, err := a.RunStream(t.Context(), "go", WithThread(id), RunWithMaxIter(1))
					if err != nil {
						t.Fatal(err)
					}
					for event, err := range seq {
						if err != nil {
							t.Fatal(err)
						}
						if _, ok := event.(ToolCallEvent); ok {
							break
						}
					}
				} else {
					result := runOptionInvoke(t, a, false, WithThread(id), RunWithMaxIter(1))
					if !result.Truncated || result.ToolCalls != 2 {
						t.Fatalf("result=%+v", result)
					}
					thread = result.ThreadID
				}
				if thread == "" || (id != "" && thread != id) {
					t.Fatalf("thread=%q, requested %q", thread, id)
				}
				equalKinds(t, logKinds(t, st, thread), store.KindRunStarted, store.KindRoundDeclared, store.KindRoundCommitted, store.KindAgentEvent)
				replayed, err := a.replayThread(t.Context(), thread)
				if err != nil {
					t.Fatal(err)
				}
				results := historyResultBlocks(replayed.history)
				if len(results) != 2 || !results[0].IsError || !results[1].IsError ||
					!strings.Contains(results[0].Content, "iteration limit") {
					t.Fatalf("durable skipped results=%+v", results)
				}
				if _, err := a.ResumeThread(t.Context(), thread); !errors.Is(err, ErrNothingToResume) {
					t.Fatalf("ResumeThread=%v", err)
				}
			})
		}
	}
}

func TestRunOptionsFinishRoundBoundary(t *testing.T) {
	for _, tc := range []struct {
		name  string
		base  int
		run   int
		tools bool
	}{
		{name: "budget one text", base: 30, run: 1},
		{name: "smaller than constructor", base: 30, run: 3},
		{name: "larger than constructor", base: 1, run: 3},
		{name: "budget one tools", base: 30, run: 1, tools: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hooked []int
			approvals, executed := 0, 0
			reg := tool.NewRegistry()
			reg.MustRegister(&parallelTestTool{
				info: tool.ToolInfo{Name: "echo", RequiresApproval: true},
				execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
					executed++
					return tool.NewTextResult("ok"), nil
				},
			})
			rounds := make([][]llm.Chunk, tc.run)
			for i := range rounds {
				rounds[i] = textChunks("continue")
			}
			if tc.tools {
				rounds = runOptionToolRounds(tc.run)
			}
			p := NewMockStreamingProvider(rounds)
			a := New(p, reg, WithMaxIter(tc.base), WithLogger(discardLogger()),
				WithApprovalFn(func(tool.ToolInfo, json.RawMessage) bool { approvals++; return true }),
				WithFinishRound(func(_ context.Context, fc FinishRoundContext) (FinishRoundDecision, error) {
					hooked = append(hooked, fc.Round)
					return FinishRoundDecision{Action: FinishRoundContinue}, nil
				}))
			result := runOptionInvoke(t, a, true, RunWithMaxIter(tc.run))
			var want []int
			for i := 0; i < tc.run-1; i++ {
				want = append(want, i)
			}
			if !reflect.DeepEqual(hooked, want) || p.index != tc.run || result.Truncated != tc.tools || approvals != 0 || executed != 0 {
				t.Fatalf("hooked=%v requests=%d truncated=%v approvals=%d executions=%d", hooked, p.index, result.Truncated, approvals, executed)
			}
			if tc.tools {
				if results := historyResultBlocks(result.History); len(results) != 1 || !results[0].IsError {
					t.Fatalf("terminal tool results=%+v", results)
				}
			}
		})
	}
}

func TestRunOptionsConflictAndErrorPrecedence(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, id := range []string{"named", "", strings.Repeat("x", 300)} {
			for _, opt := range []struct {
				name  string
				value RunOption
			}{
				{name: "llm", value: RunWithLLMOptions(llm.WithModel("other"))},
				{name: "empty llm", value: RunWithLLMOptions()},
				{name: "prompt", value: RunWithSystemPrompt("other")},
				{name: "empty prompt", value: RunWithSystemPrompt("")},
				{name: "no conflict"},
			} {
				for _, configured := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/id=%q/store=%v/stream=%v", opt.name, id, configured, stream), func(t *testing.T) {
						st := &storeAccessGuard{Store: store.NewMemory()}
						p := NewMockStreamingProvider(nil)
						base := []Option{WithLogger(discardLogger())}
						if configured {
							base = append(base, WithStore(st))
						}
						opts := []RunOption{WithThread(id)}
						want := ErrNoStore
						if opt.value != nil {
							opts = append(opts, opt.value)
							if configured {
								want = ErrRunOptionConflict
							}
						} else if configured {
							if len(id) < 300 {
								return // Valid routes are exercised by the durable table.
							}
							want = ErrInvalidThreadID
						}
						a := New(p, tool.NewRegistry(), base...)
						var err error
						if stream {
							seq, e := a.RunStream(t.Context(), "go", opts...)
							err = e
							if seq != nil {
								t.Fatal("synchronous failure returned iterator")
							}
						} else {
							res, e := a.Run(t.Context(), "go", opts...)
							err = e
							if res != nil {
								t.Fatal("synchronous failure returned result")
							}
						}
						if !errors.Is(err, want) || st.accesses != 0 || len(p.AllMessages) != 0 {
							t.Fatalf("error=%v want=%v store=%d provider=%d", err, want, st.accesses, len(p.AllMessages))
						}
					})
				}
			}
		}
	}
}

type storeAccessGuard struct {
	store.Store
	accesses int
}

func TestRunOptionsResumeAndHistoryBudgets(t *testing.T) {
	for _, failure := range []string{"provider", "truncated Done"} {
		t.Run(failure, func(t *testing.T) {
			st := &failAppendStore{Store: store.NewMemory()}
			rounds := runOptionToolRounds(4)
			p := NewMockStreamingProvider(rounds)
			if failure == "provider" {
				p.streamErr = errors.New("provider failed")
			} else {
				st.failOnCall = 4 // run_started, declaration, skipped commit, Done.
			}
			executed := 0
			reg := tool.NewRegistry()
			reg.MustRegister(mutatorTool("echo", func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
				executed++
				return tool.NewTextResult("ok"), nil
			}))
			var logicalRounds []int
			a := New(p, reg, WithStore(st), WithMaxIter(3), WithLogger(discardLogger()),
				WithFinishRound(func(_ context.Context, fc FinishRoundContext) (FinishRoundDecision, error) {
					logicalRounds = append(logicalRounds, fc.Round)
					return FinishRoundDecision{}, nil
				}))
			if _, err := a.Run(t.Context(), "go", WithThread("thread"), RunWithMaxIter(1)); err == nil {
				t.Fatal("injected failure did not interrupt run")
			}
			before := p.index
			p.streamErr = nil
			result, err := a.ResumeThread(t.Context(), "thread")
			if err != nil {
				t.Fatal(err)
			}
			if p.index-before != 3 || executed != 2 || !result.Truncated {
				t.Fatalf("resumed requests/executions/truncated=%d/%d/%v", p.index-before, executed, result.Truncated)
			}
			wantRounds := []int{0, 1}
			wantResults := 3
			if failure == "truncated Done" {
				wantResults = 4
				wantRounds = []int{1, 2}
			}
			results := historyResultBlocks(result.History)
			if !reflect.DeepEqual(logicalRounds, wantRounds) || len(results) != wantResults {
				t.Fatalf("rounds=%v results=%d, want %v/%d", logicalRounds, len(results), wantRounds, wantResults)
			}
			if failure == "truncated Done" && (!results[0].IsError || !strings.Contains(results[0].Content, "iteration limit")) {
				t.Fatalf("previous skipped result changed: %+v", results[0])
			}
			replayed, err := a.replayThread(t.Context(), "thread")
			if err != nil || !reflect.DeepEqual(result.History, replayed.history) {
				t.Fatalf("resume history differs from log: %v", err)
			}
		})
	}
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("history/stream=%v", stream), func(t *testing.T) {
			p := NewMockStreamingProvider(runOptionToolRounds(3))
			a := New(p, tool.NewRegistry(), WithMaxIter(3), WithSystemPrompt("must not inject"), WithLogger(discardLogger()))
			history := []llm.Message{llm.SystemMessage("imported")}
			var result *RunResult
			var err error
			if stream {
				seq, e := a.RunStreamWithHistory(t.Context(), history, "task")
				if e != nil {
					t.Fatal(e)
				}
				result, err = a.foldRunStream(seq)
			} else {
				result, err = a.RunWithHistory(t.Context(), history, "task")
			}
			if err != nil {
				t.Fatal(err)
			}
			if p.index != 3 || !result.Truncated || !reflect.DeepEqual(result.History[:1], history) {
				t.Fatalf("history requests=%d result=%+v", p.index, result)
			}
		})
	}
}

func (s *storeAccessGuard) Latest(ctx context.Context, thread string) (store.ThreadState, error) {
	s.accesses++
	return s.Store.Latest(ctx, thread)
}

func (s *storeAccessGuard) History(ctx context.Context, thread string, from int64) ([]store.Record, error) {
	s.accesses++
	return s.Store.History(ctx, thread, from)
}

func (s *storeAccessGuard) Append(ctx context.Context, thread string, rev int64, records ...store.Record) (int64, error) {
	s.accesses++
	return s.Store.Append(ctx, thread, rev, records...)
}
