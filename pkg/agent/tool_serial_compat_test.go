package agent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/dailz1/go-agent/pkg/llm"
	"github.com/dailz1/go-agent/pkg/tool"
)

func TestSerialPlannerInfoPanicEscapes(t *testing.T) {
	panicTool := &plannerInfoPanicTool{}
	reg := tool.NewRegistry()
	reg.MustRegister(panicTool)
	provider := &plannerArmProvider{MockStreamingProvider: parallelProvider(parallelToolCalls("panic_info")), arm: func() { panicTool.armed.Store(true) }}
	seq, err := New(provider, reg, WithToolConcurrency(1), WithLogger(discardLogger())).RunStream(context.Background(), "go")
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	defer func() {
		if recovered := recover(); recovered == nil {
			t.Fatal("serial Info panic did not escape")
		}
	}()
	for range seq {
	}
}

func TestSerialApprovalPanicEscapes(t *testing.T) {
	reg := tool.NewRegistry()
	reg.MustRegister(&parallelTestTool{info: tool.ToolInfo{Name: "approval", RequiresApproval: true}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult("wrong"), nil
	}})
	seq, err := New(parallelProvider(parallelToolCalls("approval")), reg, WithToolConcurrency(1), WithApprovalFn(func(tool.ToolInfo, json.RawMessage) bool { panic("approval") }), WithLogger(discardLogger())).RunStream(context.Background(), "go")
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	defer func() {
		if recovered := recover(); recovered == nil {
			t.Fatal("serial approval panic did not escape")
		}
	}()
	for range seq {
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
