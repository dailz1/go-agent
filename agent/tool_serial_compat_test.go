package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

// serialBoundaryInfoTool arms behavior at the serial Info boundary: onInfo
// fires on every read, and infoCalls must be compared against a baseline
// taken after registration, because Registry.Register also reads Info.
type serialBoundaryInfoTool struct {
	info         tool.ToolInfo
	onInfo       func()
	result       *tool.ToolResult
	infoCalls    atomic.Int32
	executeCalls atomic.Int32
	lastCtxErr   error
}

func (t *serialBoundaryInfoTool) Info() tool.ToolInfo {
	t.infoCalls.Add(1)
	if t.onInfo != nil {
		t.onInfo()
	}
	return t.info
}

func (t *serialBoundaryInfoTool) Execute(ctx context.Context, _ json.RawMessage) (*tool.ToolResult, error) {
	t.executeCalls.Add(1)
	t.lastCtxErr = ctx.Err()
	if t.result != nil {
		return t.result, nil
	}
	return tool.NewTextResult("ok"), nil
}

// drainStream consumes the whole sequence, continuing after errors: settle
// must stop the iterator on its own even when the consumer accepts the
// terminal error. The trace is chronological — prefix results appear before
// the terminal error entry — so delivery order is pinned, not just content.
func drainStream(seq iter.Seq2[AgentEvent, error]) (trace []string, errs []error, sawDone bool) {
	for event, err := range seq {
		if err != nil {
			errs = append(errs, err)
			trace = append(trace, "error:"+err.Error())
			continue
		}
		switch typed := event.(type) {
		case ToolCallEvent:
			trace = append(trace, "call:"+typed.ID)
		case ToolResultEvent:
			trace = append(trace, "result:"+typed.ID+":"+typed.Result.Content)
		case DoneEvent:
			sawDone = true
			trace = append(trace, "done")
		}
	}
	return trace, errs, sawDone
}

// newSerialInfoPanicAgent builds an agent whose declared calls are exactly
// calls, with a runtime-armed Info panic on the "panic_info" tool.
func newSerialInfoPanicAgent(t *testing.T, calls []string, options []Option) (*Agent, *plannerInfoPanicTool, *plannerArmProvider) {
	t.Helper()
	panicTool := &plannerInfoPanicTool{}
	reg := tool.NewRegistry()
	reg.MustRegister(&parallelTestTool{info: tool.ToolInfo{Name: "one"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult("one"), nil
	}})
	reg.MustRegister(panicTool)
	reg.MustRegister(&parallelTestTool{info: tool.ToolInfo{Name: "three"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult("wrong"), nil
	}})
	provider := &plannerArmProvider{
		MockStreamingProvider: parallelProvider(parallelToolCalls(calls...)),
		arm:                   func() { panicTool.armed.Store(true) },
	}
	opts := append([]Option{}, options...)
	opts = append(opts, WithLogger(discardLogger()))
	return New(provider, reg, opts...), panicTool, provider
}

func newSerialApprovalPanicAgent(t *testing.T, options []Option) *Agent {
	t.Helper()
	reg := tool.NewRegistry()
	reg.MustRegister(&parallelTestTool{info: tool.ToolInfo{Name: "one"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult("one"), nil
	}})
	reg.MustRegister(&parallelTestTool{info: tool.ToolInfo{Name: "approval", RequiresApproval: true}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult("wrong"), nil
	}})
	reg.MustRegister(&parallelTestTool{info: tool.ToolInfo{Name: "three"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult("wrong"), nil
	}})
	opts := append([]Option{}, options...)
	opts = append(opts,
		WithApprovalFn(func(tool.ToolInfo, json.RawMessage) bool { panic("approval") }),
		WithLogger(discardLogger()),
	)
	return New(parallelProvider(parallelToolCalls("one", "approval", "three")), reg, opts...)
}

func countResults(trace []string) int {
	count := 0
	for _, entry := range trace {
		if strings.HasPrefix(entry, "result:") {
			count++
		}
	}
	return count
}

func TestSerialPlannerInfoPanicContained(t *testing.T) {
	for name, calls := range map[string][]string{
		"panic_first":        {"panic_info"},
		"success_then_panic": {"one", "panic_info", "three"},
	} {
		t.Run(name, func(t *testing.T) {
			agent, panicTool, provider := newSerialInfoPanicAgent(t, calls, []Option{WithToolConcurrency(1)})
			seq, err := agent.RunStream(context.Background(), "go")
			if err != nil {
				t.Fatalf("RunStream: %v", err)
			}
			trace, errs, sawDone := drainStream(seq)
			const want = `iteration 0: tool "panic_info": tool info panicked: info`
			if len(errs) != 1 || errs[0].Error() != want {
				t.Fatalf("errors = %v, want exactly %q", errs, want)
			}
			if sawDone {
				t.Error("DoneEvent delivered after hard error")
			}
			if panicTool.execCalls.Load() != 0 {
				t.Fatal("Info panic executed its tool")
			}
			panicIndex := 0
			for index, call := range calls {
				if call == "panic_info" {
					panicIndex = index
				}
			}
			if got := countResults(trace); got != panicIndex {
				t.Fatalf("results = %d, want %d before the panic slot", got, panicIndex)
			}
			if provider.index != 1 {
				t.Errorf("provider calls = %d, want 1", provider.index)
			}
		})
	}
}

func TestSerialApprovalPanicContained(t *testing.T) {
	for name, calls := range map[string][]string{
		"panic_first":        {"approval"},
		"success_then_panic": {"one", "approval", "three"},
	} {
		t.Run(name, func(t *testing.T) {
			reg := tool.NewRegistry()
			one := &parallelTestTool{info: tool.ToolInfo{Name: "one"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
				return tool.NewTextResult("one"), nil
			}}
			approval := &parallelTestTool{info: tool.ToolInfo{Name: "approval", RequiresApproval: true}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
				return tool.NewTextResult("wrong"), nil
			}}
			three := &parallelTestTool{info: tool.ToolInfo{Name: "three"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
				return tool.NewTextResult("wrong"), nil
			}}
			reg.MustRegister(one)
			reg.MustRegister(approval)
			reg.MustRegister(three)
			var approvals atomic.Int32
			opts := []Option{
				WithToolConcurrency(1),
				WithApprovalFn(func(tool.ToolInfo, json.RawMessage) bool {
					approvals.Add(1)
					panic("approval")
				}),
				WithLogger(discardLogger()),
			}
			seq, err := New(parallelProvider(parallelToolCalls(calls...)), reg, opts...).RunStream(context.Background(), "go")
			if err != nil {
				t.Fatalf("RunStream: %v", err)
			}
			trace, errs, sawDone := drainStream(seq)
			const want = `iteration 0: tool "approval": approval callback panicked: approval`
			if len(errs) != 1 || errs[0].Error() != want {
				t.Fatalf("errors = %v, want exactly %q", errs, want)
			}
			if sawDone {
				t.Error("DoneEvent delivered after hard error")
			}
			if approvals.Load() != 1 {
				t.Errorf("approval callbacks = %d, want 1", approvals.Load())
			}
			if approval.calls.Load() != 0 || three.calls.Load() != 0 {
				t.Fatal("approval panic executed a tool")
			}
			if name == "success_then_panic" && one.calls.Load() != 1 {
				t.Fatalf("prefix executes = %d, want 1", one.calls.Load())
			}
			panicIndex := 0
			for index, call := range calls {
				if call == "approval" {
					panicIndex = index
				}
			}
			if got := countResults(trace); got != panicIndex {
				t.Fatalf("results = %d, want %d before the panic slot", got, panicIndex)
			}
		})
	}
}

func TestDefaultAndExplicitSerialMatchPlannerPanics(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		info   bool
		viaRun bool
	}{
		{"info/run", true, true},
		{"info/stream", true, false},
		{"approval/run", false, true},
		{"approval/stream", false, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var runErrs []string
			var traces [][]string
			for _, options := range [][]Option{nil, {WithToolConcurrency(1)}} {
				var agent *Agent
				if scenario.info {
					agent, _, _ = newSerialInfoPanicAgent(t, []string{"one", "panic_info", "three"}, options)
				} else {
					agent = newSerialApprovalPanicAgent(t, options)
				}
				if scenario.viaRun {
					result, err := agent.Run(context.Background(), "go")
					if err == nil {
						t.Fatal("Run accepted planner panic")
					}
					if result != nil {
						t.Fatalf("Run returned result %#v with error", result)
					}
					runErrs = append(runErrs, err.Error())
					continue
				}
				seq, err := agent.RunStream(context.Background(), "go")
				if err != nil {
					t.Fatalf("RunStream: %v", err)
				}
				trace, errs, sawDone := drainStream(seq)
				if sawDone || len(errs) != 1 {
					t.Fatalf("stream errors = %v, sawDone = %v", errs, sawDone)
				}
				traces = append(traces, trace)
			}
			if scenario.viaRun && (len(runErrs) != 2 || runErrs[0] != runErrs[1]) {
				t.Fatalf("run errors differ: %v", runErrs)
			}
			if !scenario.viaRun && (len(traces) != 2 || !reflect.DeepEqual(traces[0], traces[1])) {
				t.Fatalf("stream traces differ: %#v vs %#v", traces[0], traces[1])
			}
		})
	}
}

func TestSerialPlannerBoundaryBehavior(t *testing.T) {
	t.Run("cancel_before_slot", func(t *testing.T) {
		reg := tool.NewRegistry()
		one := &parallelTestTool{info: tool.ToolInfo{Name: "one"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
			return tool.NewTextResult("one"), nil
		}}
		two := &parallelTestTool{info: tool.ToolInfo{Name: "two"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
			return tool.NewTextResult("two"), nil
		}}
		reg.MustRegister(one)
		reg.MustRegister(two)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		seq, err := New(parallelProvider(parallelToolCalls("one", "two")), reg, WithToolConcurrency(1), WithLogger(discardLogger())).RunStream(ctx, "go")
		if err != nil {
			t.Fatalf("RunStream: %v", err)
		}
		var delivered atomic.Int32
		var errs []error
		results := 0
		sawDone := false
		for event, streamErr := range seq {
			if streamErr != nil {
				errs = append(errs, streamErr)
				continue
			}
			switch event.(type) {
			case ToolCallEvent:
				if delivered.Add(1) == 2 {
					cancel()
				}
			case ToolResultEvent:
				results++
			case DoneEvent:
				sawDone = true
			}
		}
		if len(errs) != 1 || !errors.Is(errs[0], context.Canceled) {
			t.Fatalf("errors = %v, want context.Canceled", errs)
		}
		if sawDone || results != 0 || delivered.Load() != 2 {
			t.Fatalf("done = %v, results = %d, calls = %d; want canceled before any slot ran", sawDone, results, delivered.Load())
		}
		if one.calls.Load() != 0 || two.calls.Load() != 0 {
			t.Fatalf("executes = %d/%d, want 0/0", one.calls.Load(), two.calls.Load())
		}
	})

	t.Run("cancel_during_info", func(t *testing.T) {
		reg := tool.NewRegistry()
		reg.MustRegister(&parallelTestTool{info: tool.ToolInfo{Name: "one"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
			return tool.NewTextResult("one"), nil
		}})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		boundary := &serialBoundaryInfoTool{info: tool.ToolInfo{Name: "boundary", RequiresApproval: true}}
		reg.MustRegister(boundary)
		boundary.onInfo = cancel
		var approvals atomic.Int32
		seq, err := New(parallelProvider(parallelToolCalls("one", "boundary")), reg, WithToolConcurrency(1),
			WithApprovalFn(func(tool.ToolInfo, json.RawMessage) bool {
				approvals.Add(1)
				return true
			}), WithLogger(discardLogger())).RunStream(ctx, "go")
		if err != nil {
			t.Fatalf("RunStream: %v", err)
		}
		_, errs, sawDone := drainStream(seq)
		if approvals.Load() != 1 {
			t.Errorf("approvals = %d, want 1 (cancellation during Info must not skip approval)", approvals.Load())
		}
		if boundary.executeCalls.Load() != 1 || boundary.lastCtxErr == nil {
			t.Fatalf("boundary executes = %d, lastCtxErr = %v; want execute with canceled ctx", boundary.executeCalls.Load(), boundary.lastCtxErr)
		}
		if len(errs) != 1 || !errors.Is(errs[0], context.Canceled) || sawDone {
			t.Fatalf("errors = %v, sawDone = %v; want run ended by cancellation at the next round", errs, sawDone)
		}
	})

	t.Run("cancel_and_approval_panic", func(t *testing.T) {
		reg := tool.NewRegistry()
		reg.MustRegister(&parallelTestTool{info: tool.ToolInfo{Name: "one"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
			return tool.NewTextResult("one"), nil
		}})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		boundary := &serialBoundaryInfoTool{info: tool.ToolInfo{Name: "boundary", RequiresApproval: true}}
		reg.MustRegister(boundary)
		boundary.onInfo = cancel
		seq, err := New(parallelProvider(parallelToolCalls("one", "boundary")), reg, WithToolConcurrency(1),
			WithApprovalFn(func(tool.ToolInfo, json.RawMessage) bool { panic("approval") }), WithLogger(discardLogger())).RunStream(ctx, "go")
		if err != nil {
			t.Fatalf("RunStream: %v", err)
		}
		_, errs, sawDone := drainStream(seq)
		if len(errs) != 1 || !containsAll(errs[0].Error(), "approval callback panicked") {
			t.Fatalf("errors = %v, want the approval panic hard error", errs)
		}
		if errors.Is(errs[0], context.Canceled) {
			t.Errorf("hard error must win over cancellation, got %v", errs[0])
		}
		if sawDone || boundary.executeCalls.Load() != 0 {
			t.Fatalf("done = %v, executes = %d; want panic slot contained with zero executes", sawDone, boundary.executeCalls.Load())
		}
	})

	t.Run("not_found", func(t *testing.T) {
		seq, err := New(parallelProvider(parallelToolCalls("ghost")), tool.NewRegistry(), WithToolConcurrency(1), WithLogger(discardLogger())).RunStream(context.Background(), "go")
		if err != nil {
			t.Fatalf("RunStream: %v", err)
		}
		trace, errs, sawDone := drainStream(seq)
		if len(errs) != 0 || !sawDone {
			t.Fatalf("errors = %v, sawDone = %v; want soft not-found and completion", errs, sawDone)
		}
		if countResults(trace) != 1 {
			t.Fatalf("results = %d, want 1", countResults(trace))
		}
		for _, entry := range trace {
			if strings.HasPrefix(entry, "result:") && !contains(entry, "not found") {
				t.Fatalf("result entry = %q, want not-found soft error", entry)
			}
		}
	})

	t.Run("approval_unconfigured", func(t *testing.T) {
		reg := tool.NewRegistry()
		approval := &parallelTestTool{info: tool.ToolInfo{Name: "approval", RequiresApproval: true}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
			return tool.NewTextResult("wrong"), nil
		}}
		reg.MustRegister(approval)
		seq, err := New(parallelProvider(parallelToolCalls("approval")), reg, WithToolConcurrency(1), WithLogger(discardLogger())).RunStream(context.Background(), "go")
		if err != nil {
			t.Fatalf("RunStream: %v", err)
		}
		trace, errs, sawDone := drainStream(seq)
		if len(errs) != 0 || !sawDone || approval.calls.Load() != 0 {
			t.Fatalf("errors = %v, sawDone = %v, executes = %d; want unconfigured soft error", errs, sawDone, approval.calls.Load())
		}
		for _, entry := range trace {
			if strings.HasPrefix(entry, "result:") && !contains(entry, "requires approval but no approval callback is configured") {
				t.Fatalf("result entry = %q, want unconfigured soft error", entry)
			}
		}
	})

	t.Run("approval_denied", func(t *testing.T) {
		reg := tool.NewRegistry()
		approval := &parallelTestTool{info: tool.ToolInfo{Name: "approval", RequiresApproval: true}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
			return tool.NewTextResult("wrong"), nil
		}}
		reg.MustRegister(approval)
		seq, err := New(parallelProvider(parallelToolCalls("approval")), reg, WithToolConcurrency(1),
			WithApprovalFn(func(tool.ToolInfo, json.RawMessage) bool { return false }), WithLogger(discardLogger())).RunStream(context.Background(), "go")
		if err != nil {
			t.Fatalf("RunStream: %v", err)
		}
		trace, errs, sawDone := drainStream(seq)
		if len(errs) != 0 || !sawDone || approval.calls.Load() != 0 {
			t.Fatalf("errors = %v, sawDone = %v, executes = %d; want denial soft error", errs, sawDone, approval.calls.Load())
		}
		for _, entry := range trace {
			if strings.HasPrefix(entry, "result:") && !contains(entry, "execution rejected by user") {
				t.Fatalf("result entry = %q, want denial soft error", entry)
			}
		}
	})

	t.Run("approval_not_required", func(t *testing.T) {
		reg := tool.NewRegistry()
		plain := &parallelTestTool{info: tool.ToolInfo{Name: "plain"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
			return tool.NewTextResult("ok"), nil
		}}
		reg.MustRegister(plain)
		var approvals atomic.Int32
		seq, err := New(parallelProvider(parallelToolCalls("plain")), reg, WithToolConcurrency(1),
			WithApprovalFn(func(tool.ToolInfo, json.RawMessage) bool {
				approvals.Add(1)
				return true
			}), WithLogger(discardLogger())).RunStream(context.Background(), "go")
		if err != nil {
			t.Fatalf("RunStream: %v", err)
		}
		_, errs, sawDone := drainStream(seq)
		if len(errs) != 0 || !sawDone || plain.calls.Load() != 1 || approvals.Load() != 0 {
			t.Fatalf("errors = %v, sawDone = %v, executes = %d, approvals = %d; want no callback without RequiresApproval",
				errs, sawDone, plain.calls.Load(), approvals.Load())
		}
	})

	t.Run("info_once", func(t *testing.T) {
		reg := tool.NewRegistry()
		boundary := &serialBoundaryInfoTool{info: tool.ToolInfo{Name: "boundary", RequiresApproval: true}}
		reg.MustRegister(boundary)
		var seenInfo []string
		base := boundary.infoCalls.Load()
		seq, err := New(parallelProvider(parallelToolCalls("boundary")), reg, WithToolConcurrency(1),
			WithApprovalFn(func(info tool.ToolInfo, _ json.RawMessage) bool {
				seenInfo = append(seenInfo, info.Name)
				return true
			}), WithLogger(discardLogger())).RunStream(context.Background(), "go")
		if err != nil {
			t.Fatalf("RunStream: %v", err)
		}
		_, errs, sawDone := drainStream(seq)
		if len(errs) != 0 || !sawDone {
			t.Fatalf("errors = %v, sawDone = %v; want successful run", errs, sawDone)
		}
		if got := boundary.infoCalls.Load() - base; got != 1 {
			t.Fatalf("execution-phase Info reads = %d, want 1 (registration excluded via baseline)", got)
		}
		if !reflect.DeepEqual(seenInfo, []string{"boundary"}) {
			t.Fatalf("callback saw info = %v, want the single live read", seenInfo)
		}
	})
}

func TestSerialPlannerPanicPreservesRecordedPrefixOnResume(t *testing.T) {
	const unknownText = "outcome unknown: the run was interrupted before this tool call's result could be recorded"
	for _, infoPanic := range []bool{true, false} {
		t.Run(fmt.Sprintf("info=%v", infoPanic), func(t *testing.T) {
			var durableLogs [][]string
			for _, options := range [][]Option{nil, {WithToolConcurrency(1)}} {
				st := store.NewMemory()
				reg := tool.NewRegistry()
				one := &parallelTestTool{info: tool.ToolInfo{Name: "one"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
					return tool.NewTextResult("real-one"), nil
				}}
				three := &parallelTestTool{info: tool.ToolInfo{Name: "three"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
					return tool.NewTextResult("wrong"), nil
				}}
				reg.MustRegister(one)
				reg.MustRegister(three)
				opts := append([]Option{}, options...)
				opts = append(opts, WithStore(st), WithLogger(discardLogger()))
				var panicTool *plannerInfoPanicTool
				var provider *plannerArmProvider
				if infoPanic {
					panicTool = &plannerInfoPanicTool{}
					reg.MustRegister(panicTool)
					var requests atomic.Int32
					provider = &plannerArmProvider{
						MockStreamingProvider: parallelProvider(parallelToolCalls("one", "panic_info", "three")),
						arm: func() {
							if requests.Add(1) == 1 {
								panicTool.armed.Store(true)
							}
						},
					}
				} else {
					approval := &parallelTestTool{info: tool.ToolInfo{Name: "approval", RequiresApproval: true}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
						return tool.NewTextResult("wrong"), nil
					}}
					reg.MustRegister(approval)
					var approvals atomic.Int32
					opts = append(opts, WithApprovalFn(func(tool.ToolInfo, json.RawMessage) bool {
						if approvals.Add(1) == 1 {
							panic("approval")
						}
						return true
					}))
					provider = &plannerArmProvider{MockStreamingProvider: parallelProvider(parallelToolCalls("one", "approval", "three")), arm: func() {}}
				}
				agent := New(provider, reg, opts...)
				_, err := agent.Run(context.Background(), "go", WithThread("t"))
				var interrupted *RunInterruptedError
				if !errors.As(err, &interrupted) || interrupted.ThreadID != "t" || interrupted.Settlement == nil {
					t.Fatalf("Run error = %v, want RunInterruptedError with settlement token", err)
				}

				declaredCalls, resultCount := 0, 0
				for _, r := range logRecords(t, st, "t") {
					switch r.Kind {
					case store.KindRoundDeclared:
						p, decodeErr := decodeRoundDeclared(r)
						if decodeErr != nil {
							t.Fatalf("decode round_declared: %v", decodeErr)
						}
						for _, block := range p.Message.Content {
							if _, ok := block.(*llm.ToolUseBlock); ok {
								declaredCalls++
							}
						}
					case store.KindRoundResult:
						p, decodeErr := decodeRoundResult(r)
						if decodeErr != nil {
							t.Fatalf("decode round_result: %v", decodeErr)
						}
						if p.Index != 0 {
							t.Errorf("recorded result index = %d, want 0", p.Index)
						}
						resultCount++
					case store.KindRoundCommitted:
						t.Error("hard-error round committed")
					}
				}
				if declaredCalls != 3 {
					t.Errorf("declared calls = %d, want 3", declaredCalls)
				}
				if resultCount != 1 {
					t.Errorf("recorded results = %d, want only index 0", resultCount)
				}

				result, err := agent.ResumeThread(context.Background(), "t")
				if err != nil {
					t.Fatalf("ResumeThread: %v", err)
				}
				blocks := historyResultBlocks(result.History)
				if len(blocks) != 3 || blocks[0].ToolUseID != "c1" || blocks[1].ToolUseID != "c2" || blocks[2].ToolUseID != "c3" {
					t.Fatalf("recovered results = %#v", blocks)
				}
				if blocks[0].IsError || blocks[0].Content != "real-one" {
					t.Fatalf("prefix result = %#v, want the real recorded result", blocks[0])
				}
				if !blocks[1].IsError || blocks[1].Content != unknownText || !blocks[2].IsError || blocks[2].Content != unknownText {
					t.Fatalf("unknown results = %#v / %#v, want the exact unknown text", blocks[1], blocks[2])
				}
				if one.calls.Load() != 1 || three.calls.Load() != 0 {
					t.Fatalf("executes = %d/%d, want 1/0 across resume", one.calls.Load(), three.calls.Load())
				}
				if panicTool != nil && panicTool.execCalls.Load() != 0 {
					t.Fatal("Info panic tool executed across resume")
				}
				durableLogs = append(durableLogs, normalizedLog(t, st, "t"))
			}
			if !reflect.DeepEqual(durableLogs[0], durableLogs[1]) {
				t.Fatalf("default and explicit serial durable logs differ:\n%v\n%v", durableLogs[0], durableLogs[1])
			}
		})
	}
}

// normalizedLog projects a thread's durable log into a comparable form:
// run_started supplies the runID, record IDs lose that prefix, and payload
// occurrences of it become "RUN", so runs differing only in random identity
// compare equal.
func normalizedLog(t *testing.T, st store.Store, thread string) []string {
	t.Helper()
	var runID string
	out := make([]string, 0, 16)
	for _, r := range logRecords(t, st, thread) {
		if r.Kind == store.KindRunStarted && runID == "" {
			p, err := decodeRunStarted(r)
			if err != nil {
				t.Fatalf("decode run_started: %v", err)
			}
			runID = p.RunID
		}
		id, payload := r.ID, string(r.Payload)
		if runID != "" {
			id = strings.TrimPrefix(id, runID)
			payload = strings.ReplaceAll(payload, runID, "RUN")
		}
		out = append(out, r.Kind+"|"+id+"|"+payload)
	}
	return out
}

// recoveryCommitPayload decodes the round-0 recovery commit from a thread's
// log, unifies its runID to a fixed identity, and re-encodes it so encoding
// equality across modes can be asserted byte-for-byte.
func recoveryCommitPayload(t *testing.T, st store.Store, thread string) []byte {
	t.Helper()
	var payload []byte
	found := 0
	for _, r := range logRecords(t, st, thread) {
		if r.Kind != store.KindRoundCommitted {
			continue
		}
		p, err := decodeRoundCommitted(r)
		if err != nil {
			t.Fatalf("decode round_committed: %v", err)
		}
		if p.Round != 0 {
			continue
		}
		found++
		p.RunID = "parity-fixed"
		rec, err := encodeRecord(store.KindRoundCommitted, recordID("parity-fixed", "-c0"), p)
		if err != nil {
			t.Fatalf("encode round_committed: %v", err)
		}
		payload = rec.Payload
	}
	if found != 1 {
		t.Fatalf("round-0 commits = %d, want 1", found)
	}
	return payload
}

func TestPlannerPanicRecoveryEncodingParity(t *testing.T) {
	const unknownText = "outcome unknown: the run was interrupted before this tool call's result could be recorded"
	modes := []struct {
		name    string
		options []Option
	}{
		{"default", nil},
		{"explicit_serial", []Option{WithToolConcurrency(1)}},
		{"parallel", []Option{WithToolConcurrency(2)}},
	}
	for _, infoPanic := range []bool{true, false} {
		name := "info"
		if !infoPanic {
			name = "approval"
		}
		t.Run(name, func(t *testing.T) {
			var baseline []byte
			for _, mode := range modes {
				t.Run(mode.name, func(t *testing.T) {
					st := store.NewMemory()
					reg := tool.NewRegistry()
					reg.MustRegister(&parallelTestTool{info: tool.ToolInfo{Name: "one"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
						return tool.NewTextResult("one"), nil
					}})
					reg.MustRegister(&parallelTestTool{info: tool.ToolInfo{Name: "three"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
						return tool.NewTextResult("wrong"), nil
					}})
					opts := append([]Option{}, mode.options...)
					opts = append(opts, WithStore(st), WithLogger(discardLogger()))
					var provider *plannerArmProvider
					if infoPanic {
						panicTool := &plannerInfoPanicTool{}
						reg.MustRegister(panicTool)
						var requests atomic.Int32
						provider = &plannerArmProvider{
							MockStreamingProvider: parallelProvider(parallelToolCalls("panic_info", "one", "three")),
							arm: func() {
								if requests.Add(1) == 1 {
									panicTool.armed.Store(true)
								}
							},
						}
					} else {
						approval := &parallelTestTool{info: tool.ToolInfo{Name: "approval", RequiresApproval: true}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
							return tool.NewTextResult("wrong"), nil
						}}
						reg.MustRegister(approval)
						var approvals atomic.Int32
						opts = append(opts, WithApprovalFn(func(tool.ToolInfo, json.RawMessage) bool {
							if approvals.Add(1) == 1 {
								panic("approval")
							}
							return true
						}))
						provider = &plannerArmProvider{MockStreamingProvider: parallelProvider(parallelToolCalls("approval", "one", "three")), arm: func() {}}
					}
					agent := New(provider, reg, opts...)
					if _, err := agent.Run(context.Background(), "go", WithThread("parity")); err == nil {
						t.Fatal("Run accepted planner panic")
					}
					for _, r := range logRecords(t, st, "parity") {
						if r.Kind == store.KindRoundResult {
							t.Fatalf("first-slot panic recorded %s before recovery", store.KindRoundResult)
						}
					}
					result, err := agent.ResumeThread(context.Background(), "parity")
					if err != nil {
						t.Fatalf("ResumeThread: %v", err)
					}
					blocks := historyResultBlocks(result.History)
					if len(blocks) != 3 {
						t.Fatalf("recovered results = %#v, want three unknowns", blocks)
					}
					for index, block := range blocks {
						if !block.IsError || block.Content != unknownText {
							t.Fatalf("recovered result %d = %#v, want exact unknown text", index, block)
						}
					}
					payload := recoveryCommitPayload(t, st, "parity")
					if baseline == nil {
						baseline = payload
						return
					}
					if !bytes.Equal(baseline, payload) {
						t.Fatalf("recovery commit payload differs across modes:\n%s\n%s", baseline, payload)
					}
				})
			}
		})
	}
}

func TestDefaultAndExplicitSerialMatchSuccessAndHardError(t *testing.T) {
	var histories [][]llm.Message
	var hardTraces [][]string
	for _, options := range [][]Option{nil, {WithToolConcurrency(1)}} {
		reg := tool.NewRegistry()
		reg.MustRegister(&parallelTestTool{info: tool.ToolInfo{Name: "one"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
			return tool.NewTextResult("one"), nil
		}})
		result, err := New(parallelProvider(parallelToolCalls("one")), reg, append(options, WithLogger(discardLogger()))...).Run(context.Background(), "go")
		if err != nil {
			t.Fatalf("success Run: %v", err)
		}
		histories = append(histories, result.History)
		reg = tool.NewRegistry()
		reg.MustRegister(&parallelTestTool{info: tool.ToolInfo{Name: "one"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
			return tool.NewTextResult("one"), nil
		}})
		reg.MustRegister(&parallelTestTool{info: tool.ToolInfo{Name: "two"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) { return nil, errors.New("boom") }})
		seq, err := New(parallelProvider(parallelToolCalls("one", "two")), reg, append(options, WithLogger(discardLogger()))...).RunStream(context.Background(), "go")
		if err != nil {
			t.Fatalf("hard-error RunStream: %v", err)
		}
		events, errs := collectEvents(t, seq, nil)
		if len(errs) != 1 {
			t.Fatalf("hard errors = %v", errs)
		}
		trace := []string{errs[0].Error()}
		for _, event := range events {
			switch typed := event.(type) {
			case ToolCallEvent:
				trace = append(trace, "call:"+typed.ID)
			case ToolResultEvent:
				trace = append(trace, "result:"+typed.ID+":"+typed.Result.Content)
			}
		}
		hardTraces = append(hardTraces, trace)
	}
	if !reflect.DeepEqual(histories[0], histories[1]) || !reflect.DeepEqual(hardTraces[0], hardTraces[1]) {
		t.Fatalf("default and explicit serial traces differ: %#v / %#v", histories, hardTraces)
	}
}

func TestDefaultAndExplicitSerialMatchApprovalInterleaving(t *testing.T) {
	var traces [][]llm.Message
	for _, options := range [][]Option{nil, {WithToolConcurrency(1)}} {
		var executed atomic.Bool
		reg := tool.NewRegistry()
		reg.MustRegister(&parallelTestTool{info: tool.ToolInfo{Name: "one"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
			executed.Store(true)
			return tool.NewTextResult("one"), nil
		}})
		reg.MustRegister(&parallelTestTool{info: tool.ToolInfo{Name: "two", RequiresApproval: true}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
			return tool.NewTextResult("two"), nil
		}})
		result, err := New(parallelProvider(parallelToolCalls("one", "two")), reg, append(options, WithApprovalFn(func(tool.ToolInfo, json.RawMessage) bool { return executed.Load() }), WithLogger(discardLogger()))...).Run(context.Background(), "go")
		if err != nil || len(historyResultBlocks(result.History)) != 2 || historyResultBlocks(result.History)[1].IsError {
			t.Fatalf("approval interleaving result = %#v, err = %v", result, err)
		}
		traces = append(traces, result.History)
	}
	if !reflect.DeepEqual(traces[0], traces[1]) {
		t.Fatalf("default and explicit approval traces differ: %#v", traces)
	}
}
