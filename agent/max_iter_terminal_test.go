package agent

import (
	"context"
	"encoding/json"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

func terminalDone(t *testing.T, events []AgentEvent) DoneEvent {
	t.Helper()
	done, ok := events[len(events)-1].(DoneEvent)
	if !ok {
		t.Fatalf("last event = %T, want DoneEvent", events[len(events)-1])
	}
	return done
}

func terminalEventIDs(t *testing.T, events []AgentEvent) (calls, results []string) {
	t.Helper()
	for _, event := range events {
		switch e := event.(type) {
		case ToolCallEvent:
			calls = append(calls, e.ID)
		case ToolResultEvent:
			results = append(results, e.ID)
		}
	}
	return calls, results
}

func TestMaxIterTerminalOrderAndCounts(t *testing.T) {
	tests := []struct {
		name       string
		rounds     [][]llm.Chunk
		maxIter    int
		wantCalls  []string
		wantCount  int
		wantResult int
	}{
		{
			name: "assembled declaration order wins over arrival order",
			rounds: [][]llm.Chunk{{
				llm.ToolCallStartChunk{Index: 1, ID: "c2", Name: "echo"},
				llm.ToolCallArgsChunk{Index: 1, Delta: `{}`},
				llm.ToolCallStartChunk{Index: 0, ID: "c1", Name: "echo"},
				llm.ToolCallArgsChunk{Index: 0, Delta: `{}`},
				llm.DoneChunk{FinishReason: "tool_calls"},
			}},
			maxIter:    1,
			wantCalls:  []string{"c1", "c2"},
			wantCount:  2,
			wantResult: 2,
		},
		{
			name: "prior normal calls plus terminal skips",
			rounds: [][]llm.Chunk{
				{
					llm.ToolCallStartChunk{Index: 0, ID: "c0", Name: "echo"},
					llm.ToolCallArgsChunk{Index: 0, Delta: `{}`},
					llm.DoneChunk{FinishReason: "tool_calls"},
				},
				{
					llm.ToolCallStartChunk{Index: 0, ID: "c1", Name: "echo"},
					llm.ToolCallArgsChunk{Index: 0, Delta: `{}`},
					llm.ToolCallStartChunk{Index: 1, ID: "c2", Name: "echo"},
					llm.ToolCallArgsChunk{Index: 1, Delta: `{}`},
					llm.DoneChunk{FinishReason: "tool_calls"},
				},
			},
			maxIter:    2,
			wantCalls:  []string{"c0", "c1", "c2"},
			wantCount:  3,
			wantResult: 3,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var executed atomic.Int32
			registry := tool.NewRegistry()
			registry.MustRegister(&parallelTestTool{
				info: tool.ToolInfo{Name: "echo"},
				execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
					executed.Add(1)
					return tool.NewTextResult("normal"), nil
				},
			})
			agent := New(
				NewMockStreamingProvider(test.rounds),
				registry,
				WithMaxIter(test.maxIter),
				WithLogger(discardLogger()),
			)
			seq, err := agent.RunStream(context.Background(), "go")
			events, errs := collectEvents(t, seq, err)
			if len(errs) != 0 {
				t.Fatalf("RunStream errors = %v", errs)
			}
			gotCalls, gotResults := terminalEventIDs(t, events)
			if !reflect.DeepEqual(gotCalls, test.wantCalls) {
				t.Errorf("ToolCall IDs = %v, want %v", gotCalls, test.wantCalls)
			}
			if !reflect.DeepEqual(gotResults, test.wantCalls) {
				t.Errorf("ToolResult IDs = %v, want %v", gotResults, test.wantCalls)
			}
			done := terminalDone(t, events)
			if !done.Truncated || done.ToolCalls != test.wantCount {
				t.Errorf("Done = truncated %v, ToolCalls %d; want true, %d", done.Truncated, done.ToolCalls, test.wantCount)
			}
			results := historyResultBlocks(done.History)
			if len(results) != test.wantResult {
				t.Fatalf("history result count = %d, want %d", len(results), test.wantResult)
			}
			wantExecuted := int32(test.maxIter - 1)
			if got := executed.Load(); got != wantExecuted {
				t.Errorf("Execute calls = %d, want %d", got, wantExecuted)
			}
		})
	}
}

func TestMaxIterTerminalTextOnlyAndNormalization(t *testing.T) {
	t.Run("terminal text only is a normal completion", func(t *testing.T) {
		agent := New(NewMockStreamingProvider([][]llm.Chunk{{
			llm.TextDeltaChunk{Text: "complete"},
			llm.DoneChunk{FinishReason: "stop"},
		}}), tool.NewRegistry(), WithMaxIter(1), WithLogger(discardLogger()))
		seq, err := agent.RunStream(context.Background(), "go")
		events, errs := collectEvents(t, seq, err)
		if len(errs) != 0 {
			t.Fatalf("RunStream errors = %v", errs)
		}
		if len(events) != 2 {
			t.Fatalf("event count = %d, want text and Done", len(events))
		}
		done := terminalDone(t, events)
		if done.Truncated || done.ToolCalls != 0 || len(historyResultBlocks(done.History)) != 0 {
			t.Errorf("text-only Done = %#v, want normal zero-call completion", done)
		}
	})

	for _, maxIter := range []int{0, -1} {
		t.Run("normalizes to ten rounds", func(t *testing.T) {
			var executed atomic.Int32
			registry := tool.NewRegistry()
			registry.MustRegister(&parallelTestTool{
				info: tool.ToolInfo{Name: "echo"},
				execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
					executed.Add(1)
					return tool.NewTextResult("normal"), nil
				},
			})
			rounds := make([][]llm.Chunk, 10)
			for index := range rounds {
				rounds[index] = parallelToolRound([]llm.ToolUseBlock{{
					Type: "tool_use", ID: string(rune('a' + index)), Name: "echo", Input: json.RawMessage(`{}`),
				}})
			}
			result, err := New(NewMockStreamingProvider(rounds), registry, WithMaxIter(maxIter), WithLogger(discardLogger())).Run(context.Background(), "go")
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if !result.Truncated || result.ToolCalls != 10 || executed.Load() != 9 {
				t.Errorf("maxIter=%d result = %#v, executions=%d; want ten calls, nine executions", maxIter, result, executed.Load())
			}
		})
	}
}

func TestMaxIterTerminalSkipsApprovalAndParallelExecution(t *testing.T) {
	var approvals, executions atomic.Int32
	registry := tool.NewRegistry()
	for _, name := range []string{"one", "two"} {
		name := name
		registry.MustRegister(&parallelTestTool{
			info: tool.ToolInfo{Name: name, RequiresApproval: true},
			execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
				executions.Add(1)
				return tool.NewTextResult(name), nil
			},
		})
	}
	agent := New(
		NewMockStreamingProvider([][]llm.Chunk{parallelToolRound(parallelToolCalls("one", "two"))}),
		registry,
		WithMaxIter(1),
		WithToolConcurrency(2),
		WithApprovalFn(func(tool.ToolInfo, json.RawMessage) bool {
			approvals.Add(1)
			return true
		}),
		WithLogger(discardLogger()),
	)
	seq, err := agent.RunStream(context.Background(), "go")
	events, errs := collectEvents(t, seq, err)
	if len(errs) != 0 {
		t.Fatalf("RunStream errors = %v", errs)
	}
	calls, results := terminalEventIDs(t, events)
	if !reflect.DeepEqual(calls, []string{"c1", "c2"}) || !reflect.DeepEqual(results, calls) {
		t.Errorf("terminal pair order = calls %v results %v", calls, results)
	}
	if approvals.Load() != 0 || executions.Load() != 0 {
		t.Errorf("approval calls=%d executions=%d, want zero", approvals.Load(), executions.Load())
	}
}
