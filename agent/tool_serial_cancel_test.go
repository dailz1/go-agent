package agent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/dailz1/go-agent/tool"
)

func TestDefaultAndExplicitSerialMatchCancellation(t *testing.T) {
	type cancellationTrace struct {
		events []string
		err    string
		calls  int32
	}
	var traces []cancellationTrace
	for _, options := range [][]Option{nil, {WithToolConcurrency(1)}} {
		reg := tool.NewRegistry()
		toolCall := &parallelTestTool{info: tool.ToolInfo{Name: "one"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
			return tool.NewTextResult("one"), nil
		}}
		reg.MustRegister(toolCall)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		seq, err := New(parallelProvider(parallelToolCalls("one")), reg, append(options, WithLogger(discardLogger()))...).RunStream(ctx, "go")
		if err != nil {
			t.Fatalf("cancel RunStream: %v", err)
		}
		trace := cancellationTrace{}
		for event, streamErr := range seq {
			if streamErr != nil {
				trace.err = streamErr.Error()
				continue
			}
			trace.events = append(trace.events, event.(ToolCallEvent).ID)
			cancel()
		}
		if !errors.Is(ctx.Err(), context.Canceled) || trace.err != context.Canceled.Error() || toolCall.calls.Load() != 0 {
			t.Fatalf("cancellation trace = %#v, calls = %d", trace, toolCall.calls.Load())
		}
		trace.calls = toolCall.calls.Load()
		traces = append(traces, trace)
	}
	if !reflect.DeepEqual(traces[0], traces[1]) {
		t.Fatalf("default and explicit cancellation traces differ: %#v", traces)
	}
}
