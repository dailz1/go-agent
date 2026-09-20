package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

type parallelTestTool struct {
	info    tool.ToolInfo
	execute func(context.Context, json.RawMessage) (*tool.ToolResult, error)
	calls   atomic.Int32
}

func (t *parallelTestTool) Info() tool.ToolInfo { return t.info }

func (t *parallelTestTool) Execute(ctx context.Context, args json.RawMessage) (*tool.ToolResult, error) {
	t.calls.Add(1)
	return t.execute(ctx, args)
}

func parallelToolCalls(names ...string) []llm.ToolUseBlock {
	calls := make([]llm.ToolUseBlock, len(names))
	for index, name := range names {
		calls[index] = llm.ToolUseBlock{Type: "tool_use", ID: fmt.Sprintf("c%d", index+1), Name: name, Input: json.RawMessage(`{}`)}
	}
	return calls
}

func parallelProvider(calls []llm.ToolUseBlock) *MockStreamingProvider {
	return NewMockStreamingProvider([][]llm.Chunk{parallelToolRound(calls), {
		llm.TextDeltaChunk{Text: "done"}, llm.DoneChunk{FinishReason: "stop"},
	}})
}

func parallelToolRound(calls []llm.ToolUseBlock) []llm.Chunk {
	chunks := make([]llm.Chunk, 0, len(calls)*2+1)
	for index, call := range calls {
		chunks = append(chunks,
			llm.ToolCallStartChunk{Index: index, ID: call.ID, Name: call.Name},
			llm.ToolCallArgsChunk{Index: index, Delta: string(call.Input)},
		)
	}
	return append(chunks, llm.DoneChunk{FinishReason: "tool_calls"})
}

func waitParallelSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	select {
	case <-signal:
	case <-ctx.Done():
		t.Fatal("timed out waiting for tool signal")
	}
}

func TestWithToolConcurrencyNormalizesAndOrdersParallelResults(t *testing.T) {
	for _, n := range []int{-1, 0, 1} {
		if agent := New(NewMockProvider(), tool.NewRegistry(), WithToolConcurrency(n)); agent.toolConcurrency != 1 {
			t.Errorf("WithToolConcurrency(%d) = %d, want 1", n, agent.toolConcurrency)
		}
	}

	c1Started, c2Started := make(chan struct{}), make(chan struct{})
	c1Release, c2Release := make(chan struct{}), make(chan struct{})
	reg := tool.NewRegistry()
	reg.MustRegister(&parallelTestTool{info: tool.ToolInfo{Name: "one"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		close(c1Started)
		<-c1Release
		return tool.NewTextResult("one"), nil
	}})
	reg.MustRegister(&parallelTestTool{info: tool.ToolInfo{Name: "two"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		close(c2Started)
		<-c2Release
		return tool.NewTextResult("two"), nil
	}})
	calls := parallelToolCalls("one", "two")
	agent := New(parallelProvider(calls), reg, WithToolConcurrency(2), WithLogger(discardLogger()))
	resultCh := make(chan struct {
		result *RunResult
		err    error
	}, 1)
	go func() {
		result, err := agent.Run(context.Background(), "go")
		resultCh <- struct {
			result *RunResult
			err    error
		}{result, err}
	}()
	waitParallelSignal(t, c1Started)
	waitParallelSignal(t, c2Started)
	close(c2Release)
	close(c1Release)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	select {
	case outcome := <-resultCh:
		if outcome.err != nil {
			t.Fatalf("Run: %v", outcome.err)
		}
		got := historyResultBlocks(outcome.result.History)
		if len(got) != 2 || got[0].ToolUseID != "c1" || got[1].ToolUseID != "c2" {
			t.Fatalf("result history IDs = %#v, want [c1 c2]", got)
		}
	case <-ctx.Done():
		t.Fatal("parallel run did not finish")
	}
}

type truncationSignalHandler struct{ truncated chan<- struct{} }

func (h truncationSignalHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h truncationSignalHandler) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h truncationSignalHandler) WithGroup(string) slog.Handler            { return h }
func (h truncationSignalHandler) Handle(_ context.Context, record slog.Record) error {
	if record.Message == "tool result truncated" {
		h.truncated <- struct{}{}
	}
	return nil
}

func TestParallelLimitsCompletedSlotsBeforeSlowWorkerDrains(t *testing.T) {
	slowStarted, slowRelease := make(chan struct{}), make(chan struct{})
	truncated := make(chan struct{}, 2)
	reg := tool.NewRegistry()
	reg.MustRegister(&parallelTestTool{info: tool.ToolInfo{Name: "slow"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		close(slowStarted)
		<-slowRelease
		return tool.NewTextResult("slow"), nil
	}})
	for _, name := range []string{"fast_one", "fast_two"} {
		name := name
		reg.MustRegister(&parallelTestTool{info: tool.ToolInfo{Name: name}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
			return tool.NewTextResult(strings.Repeat(name, 100)), nil
		}})
	}
	agent := New(parallelProvider(parallelToolCalls("slow", "fast_one", "fast_two")), reg,
		WithToolConcurrency(3), WithContextWindowTokens(128), WithCompactor(compactKeepSystem{}),
		WithLogger(slog.New(truncationSignalHandler{truncated: truncated})))
	outcome := make(chan error, 1)
	go func() {
		_, err := agent.Run(context.Background(), "go")
		outcome <- err
	}()
	waitParallelSignal(t, slowStarted)
	waitParallelSignal(t, truncated)
	waitParallelSignal(t, truncated)
	close(slowRelease)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	select {
	case err := <-outcome:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("parallel run did not finish after slow worker release")
	}
}

func TestParallelHardErrorsKeepOnlyTheOrderedPrefix(t *testing.T) {
	for _, test := range []struct {
		name      string
		errors    map[int]bool
		wantCount int
		wantName  string
	}{
		{name: "first", errors: map[int]bool{0: true}, wantName: "one"},
		{name: "middle", errors: map[int]bool{1: true}, wantCount: 1, wantName: "two"},
		{name: "multiple", errors: map[int]bool{0: true, 2: true}, wantName: "one"},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := parallelToolCalls("one", "two", "three")
			reg := tool.NewRegistry()
			for index, call := range calls {
				index, call := index, call
				reg.MustRegister(&parallelTestTool{info: tool.ToolInfo{Name: call.Name}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
					if test.errors[index] {
						return nil, errors.New("boom")
					}
					return tool.NewTextResult(call.Name), nil
				}})
			}
			seq, err := New(parallelProvider(calls), reg, WithToolConcurrency(3), WithLogger(discardLogger())).RunStream(context.Background(), "go")
			if err != nil {
				t.Fatalf("RunStream: %v", err)
			}
			events, errs := collectEvents(t, seq, nil)
			if len(errs) != 1 || errs[0] == nil {
				t.Fatalf("errors = %v, want one hard error", errs)
			}
			if got := errs[0].Error(); !containsAll(got, "iteration 0", test.wantName, "execute: boom") {
				t.Errorf("error = %q, want iteration/name context", got)
			}
			var resultIDs []string
			for _, event := range events {
				if result, ok := event.(ToolResultEvent); ok {
					resultIDs = append(resultIDs, result.ID)
				}
			}
			if len(resultIDs) != test.wantCount {
				t.Fatalf("result IDs = %v, want %d prefix results", resultIDs, test.wantCount)
			}
			for index, id := range resultIDs {
				if id != calls[index].ID {
					t.Errorf("result ID %d = %q, want %q", index, id, calls[index].ID)
				}
			}
		})
	}
}

func containsAll(s string, terms ...string) bool {
	for _, term := range terms {
		if !contains(s, term) {
			return false
		}
	}
	return true
}

func historyResultBlocks(history []llm.Message) []llm.ToolResultBlock {
	var results []llm.ToolResultBlock
	for _, message := range history {
		results = append(results, resultBlocks(message)...)
	}
	return results
}
