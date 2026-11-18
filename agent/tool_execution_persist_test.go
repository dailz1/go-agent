package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

// assertRecordedBeforeDelivery reads the real store inside the consumer
// callback: the delivered call's round_result must already be durable while
// the round is still open.
func assertRecordedBeforeDelivery(t *testing.T, st store.Store, thread, callID, content string) {
	t.Helper()
	for _, r := range logRecords(t, st, thread) {
		if r.Kind != store.KindRoundResult {
			continue
		}
		var p roundResultPayload
		if err := strictDecode(r.Payload, &p); err != nil {
			t.Fatalf("round_result payload: %v", err)
		}
		blocks := toolResultBlocks(p.Result)
		if len(blocks) == 1 && blocks[0].ToolUseID == callID {
			if blocks[0].Content != content {
				t.Fatalf("recorded result = %#v, want %q", blocks[0], content)
			}
			if hasKind(t, st, thread, store.KindRoundCommitted) {
				t.Fatal("round committed while results are still being delivered")
			}
			return
		}
	}
	t.Fatalf("call %q delivered without a durable round_result", callID)
}

func TestToolResultDurableBeforeDelivery(t *testing.T) {
	for _, tc := range []struct {
		name        string
		concurrency int
	}{
		{name: "serial", concurrency: 1},
		{name: "parallel_reverse_completion", concurrency: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := store.NewMemory()
			reg := tool.NewRegistry()
			secondDone := make(chan struct{})
			var once sync.Once
			releaseSecond := func() {}
			if tc.concurrency == 2 {
				// Reverse the completion order: the second declared call
				// finishes first, the first one blocks until it does.
				var wg sync.WaitGroup
				wg.Add(1)
				defer wg.Wait()
				reg.MustRegister(mutatorTool("slow_first", func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
					once.Do(func() { close(secondDone) })
					wg.Done()
					return tool.NewTextResult("first"), nil
				}))
				reg.MustRegister(mutatorTool("fast_second", func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
					<-secondDone
					return tool.NewTextResult("second"), nil
				}))
			} else {
				reg.MustRegister(mutatorTool("slow_first", func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
					return tool.NewTextResult("first"), nil
				}))
				reg.MustRegister(mutatorTool("fast_second", func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
					return tool.NewTextResult("second"), nil
				}))
			}
			_ = releaseSecond
			agent := New(parallelProvider(parallelToolCalls("slow_first", "fast_second")), reg,
				WithStore(st), WithToolConcurrency(tc.concurrency), WithLogger(discardLogger()))
			seq, err := agent.RunStream(ctx, "go", WithThread("t"))
			if err != nil {
				t.Fatalf("RunStream: %v", err)
			}
			delivered := 0
			for event, streamErr := range seq {
				if streamErr != nil {
					t.Fatalf("stream: %v", streamErr)
				}
				if ev, ok := event.(ToolResultEvent); ok {
					delivered++
					assertRecordedBeforeDelivery(t, st, "t", ev.ID, ev.Result.Content)
				}
				if _, ok := event.(DoneEvent); ok {
					break
				}
			}
			if delivered != 2 {
				t.Fatalf("delivered = %d, want 2", delivered)
			}
			if !hasKind(t, st, "t", store.KindRoundCommitted) {
				t.Fatal("a fully consumed round must end committed")
			}
		})
	}
}

func TestToolResultConsumerBreakRecovery(t *testing.T) {
	for _, tc := range []struct {
		name        string
		concurrency int
		breakFirst  bool
	}{
		{name: "serial_break_first", concurrency: 1, breakFirst: true},
		{name: "serial_break_last", concurrency: 1, breakFirst: false},
		{name: "parallel_break_first", concurrency: 2, breakFirst: true},
		{name: "parallel_break_last", concurrency: 2, breakFirst: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := store.NewMemory()
			reg := tool.NewRegistry()
			var executions atomic.Int32
			for _, name := range []string{"one", "two"} {
				name := name
				reg.MustRegister(mutatorTool(name, func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
					executions.Add(1)
					return tool.NewTextResult(name), nil
				}))
			}
			agent := New(parallelProvider(parallelToolCalls("one", "two")), reg,
				WithStore(st), WithToolConcurrency(tc.concurrency), WithLogger(discardLogger()))
			seq, err := agent.RunStream(ctx, "go", WithThread("t"))
			if err != nil {
				t.Fatalf("RunStream: %v", err)
			}
			delivered := 0
			for event, streamErr := range seq {
				if streamErr != nil {
					break
				}
				if _, ok := event.(ToolResultEvent); !ok {
					continue
				}
				delivered++
				if (tc.breakFirst && delivered == 1) || delivered == 2 {
					break
				}
			}
			before := executions.Load()
			res, err := agent.ResumeThread(ctx, "t")
			if err != nil {
				t.Fatalf("ResumeThread: %v", err)
			}
			if executions.Load() != before {
				t.Fatal("recovery re-executed tools")
			}
			blocks := historyResultBlocks(res.History)
			if len(blocks) != 2 || blocks[0].ToolUseID != "c1" || blocks[1].ToolUseID != "c2" {
				t.Fatalf("recovered results = %#v", blocks)
			}
			if got := blocks[0].IsError || !strings.Contains(blocks[0].Content, "one"); got != false {
				t.Fatalf("first result = %#v, want the delivered real result", blocks[0])
			}
			if tc.breakFirst {
				if !blocks[1].IsError || !strings.Contains(blocks[1].Content, "outcome unknown") {
					t.Fatalf("second result = %#v, want unknown after an early break", blocks[1])
				}
			} else if blocks[1].IsError || !strings.Contains(blocks[1].Content, "two") {
				t.Fatalf("second result = %#v, want the delivered real result", blocks[1])
			}
		})
	}
}

func TestToolResultHardErrorRecovery(t *testing.T) {
	for _, tc := range []struct {
		name        string
		concurrency int
		boom        string
	}{
		{name: "serial_second_hard_error", concurrency: 1, boom: "two"},
		{name: "parallel_middle_hard_error", concurrency: 3, boom: "two"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := store.NewMemory()
			reg := tool.NewRegistry()
			for _, name := range []string{"one", "two", "three"} {
				name := name
				reg.MustRegister(mutatorTool(name, func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
					if name == tc.boom {
						return nil, errors.New("boom")
					}
					return tool.NewTextResult(name), nil
				}))
			}
			agent := New(parallelProvider(parallelToolCalls("one", "two", "three")), reg,
				WithStore(st), WithToolConcurrency(tc.concurrency), WithLogger(discardLogger()))
			seq, err := agent.RunStream(ctx, "go", WithThread("t"))
			if err != nil {
				t.Fatalf("RunStream: %v", err)
			}
			hadError := false
			for _, streamErr := range seq {
				if streamErr != nil {
					hadError = true
					break
				}
			}
			if !hadError {
				t.Fatal("the hard error must abort the run")
			}
			res, err := agent.ResumeThread(ctx, "t")
			if err != nil {
				t.Fatalf("ResumeThread: %v", err)
			}
			blocks := historyResultBlocks(res.History)
			if len(blocks) != 3 {
				t.Fatalf("recovered results = %d, want one per declared call", len(blocks))
			}
			if blocks[0].IsError || !strings.Contains(blocks[0].Content, "one") {
				t.Fatalf("prefix result = %#v, want the recorded real result", blocks[0])
			}
			for i, b := range blocks[1:] {
				if !b.IsError || !strings.Contains(b.Content, "outcome unknown") {
					t.Fatalf("result %d = %#v, want unknown", i+1, b)
				}
			}
		})
	}
}

func TestToolResultWriteContext(t *testing.T) {
	ctx := context.Background()
	st := &resultFailStore{Store: store.NewMemory(), failAfter: 0}
	reg := tool.NewRegistry()
	reg.MustRegister(mutatorTool("one", func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult("one"), nil
	}))
	agent := New(parallelProvider(parallelToolCalls("one")), reg, WithStore(st), WithLogger(discardLogger()))
	seq, err := agent.RunStream(ctx, "go", WithThread("t"))
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	seen := false
	aborted := false
	for event, streamErr := range seq {
		if _, ok := event.(ToolResultEvent); ok {
			seen = true
		}
		if streamErr != nil {
			aborted = true
			break
		}
	}
	if seen || !aborted {
		t.Fatalf("seen=%v aborted=%v, want abort before delivery when the record write fails", seen, aborted)
	}
}

func TestToolResultNonPersistent(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(reg *tool.Registry) *Agent
	}{
		{name: "no_store", setup: func(reg *tool.Registry) *Agent {
			return New(parallelProvider(parallelToolCalls("one")), reg, WithLogger(discardLogger()))
		}},
		{name: "store_without_withthread", setup: func(reg *tool.Registry) *Agent {
			return New(parallelProvider(parallelToolCalls("one")), reg, WithStore(store.NewMemory()), WithLogger(discardLogger()))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := tool.NewRegistry()
			reg.MustRegister(mutatorTool("one", func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
				return tool.NewTextResult("one"), nil
			}))
			agent := tc.setup(reg)
			seq, err := agent.RunStream(context.Background(), "go")
			if err != nil {
				t.Fatalf("RunStream: %v", err)
			}
			results := 0
			for event, streamErr := range seq {
				if streamErr != nil {
					t.Fatalf("stream: %v", streamErr)
				}
				if _, ok := event.(ToolResultEvent); ok {
					results++
				}
			}
			if results != 1 {
				t.Fatalf("results = %d, want the non-persistent delivery unchanged", results)
			}
		})
	}
}
