package agent

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"reflect"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

// chunkThenErrProvider streams preset chunks, then fails mid-stream.
type chunkThenErrProvider struct {
	chunks []llm.Chunk
	err    error
}

func (p *chunkThenErrProvider) Name() string { return "chunk_then_err" }

func (p *chunkThenErrProvider) Chat(context.Context, []llm.Message, []tool.ToolInfo, ...llm.Option) (*llm.Message, *llm.Usage, error) {
	return nil, nil, llm.ErrStreamingNotSupported
}

func (p *chunkThenErrProvider) ChatStream(context.Context, []llm.Message, []tool.ToolInfo, ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	return func(yield func(llm.Chunk, error) bool) {
		for _, c := range p.chunks {
			if !yield(c, nil) {
				return
			}
		}
		yield(nil, p.err)
	}, nil
}

func TestAgentEventFoldKeepsFirstToolCallOnError(t *testing.T) {
	t.Parallel()

	call1 := llm.ToolUseBlock{Type: "tool_use", ID: "c1", Name: "t1", Input: json.RawMessage(`{}`)}
	registry := tool.NewRegistry()
	registry.MustRegister(&mockTool{info: tool.ToolInfo{Name: "t1"}, err: errors.New("boom")})
	p := NewMockStreamingProvider([][]llm.Chunk{
		{
			llm.ToolCallStartChunk{Index: 0, ID: "c1", Name: "t1"},
			llm.ToolCallArgsChunk{Index: 0, ID: "c1", Delta: `{}`},
			llm.DoneChunk{FinishReason: "tool_calls"},
		},
	})
	ag := New(p, registry, WithLogger(discardLogger()))

	seq, err := ag.RunStream(context.Background(), "go")
	if err != nil {
		t.Fatalf("RunStream returned error: %v", err)
	}
	events, errs := collectEvents(t, seq, nil)
	if len(errs) != 1 {
		t.Fatalf("stream errors = %v, want exactly one", errs)
	}

	seed := []llm.Message{llm.UserMessage("go")}
	fold := newEventFold(seed)
	for _, ev := range events {
		fold.apply(t, ev)
	}
	want := append(seed,
		llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{call1}},
	)
	if got := fold.finishErr(); !reflect.DeepEqual(got, want) {
		t.Errorf("fold after first-tool error\n got: %+v\nwant: %+v", got, want)
	}
}

func TestAgentEventFoldDiscardsPartialRoundOnStreamError(t *testing.T) {
	t.Parallel()

	p := &chunkThenErrProvider{
		chunks: []llm.Chunk{
			llm.TextDeltaChunk{Text: "par"},
			llm.TextDeltaChunk{Text: "tial"},
		},
		err: errors.New("stream broke"),
	}
	ag := New(p, tool.NewRegistry(), WithLogger(discardLogger()))

	seq, err := ag.RunStream(context.Background(), "go")
	if err != nil {
		t.Fatalf("RunStream returned error: %v", err)
	}
	events, errs := collectEvents(t, seq, nil)
	if len(errs) != 1 {
		t.Fatalf("stream errors = %v, want exactly one", errs)
	}

	seed := []llm.Message{llm.UserMessage("go")}
	fold := newEventFold(seed)
	for _, ev := range events {
		fold.apply(t, ev)
	}
	if got := fold.finishErr(); !reflect.DeepEqual(got, seed) {
		t.Errorf("fold after mid-stream error = %+v, want unchanged seed", got)
	}
}

func TestAgentEventFoldKeepsRoundWithPartialResultsOnError(t *testing.T) {
	t.Parallel()

	call1 := llm.ToolUseBlock{Type: "tool_use", ID: "c1", Name: "t1", Input: json.RawMessage(`{}`)}
	call2 := llm.ToolUseBlock{Type: "tool_use", ID: "c2", Name: "t2", Input: json.RawMessage(`{}`)}
	registry := tool.NewRegistry()
	registry.MustRegister(&mockTool{info: tool.ToolInfo{Name: "t1"}, result: tool.NewTextResult("r1")})
	registry.MustRegister(&mockTool{info: tool.ToolInfo{Name: "t2"}, err: errors.New("boom")})
	p := NewMockStreamingProvider([][]llm.Chunk{
		{
			llm.ToolCallStartChunk{Index: 0, ID: "c1", Name: "t1"},
			llm.ToolCallArgsChunk{Index: 0, ID: "c1", Delta: `{}`},
			llm.ToolCallStartChunk{Index: 1, ID: "c2", Name: "t2"},
			llm.ToolCallArgsChunk{Index: 1, ID: "c2", Delta: `{}`},
			llm.DoneChunk{FinishReason: "tool_calls"},
		},
	})
	ag := New(p, registry, WithLogger(discardLogger()))

	seq, err := ag.RunStream(context.Background(), "go")
	if err != nil {
		t.Fatalf("RunStream returned error: %v", err)
	}
	events, errs := collectEvents(t, seq, nil)
	if len(errs) != 1 {
		t.Fatalf("stream errors = %v, want exactly one", errs)
	}

	seed := []llm.Message{llm.UserMessage("go")}
	fold := newEventFold(seed)
	for _, ev := range events {
		fold.apply(t, ev)
	}
	want := append(seed,
		llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{call1, call2}},
		llm.ToolResultMessage("c1", tool.NewTextResult("r1")),
	)
	if got := fold.finishErr(); !reflect.DeepEqual(got, want) {
		t.Errorf("fold after second-tool error\n got: %+v\nwant: %+v", got, want)
	}
}

// TestRunStreamCancelAfterAnnouncedCallsRebuilds pins the cancellation
// contract of TC-first emission: cancellation after the announcements,
// before execution begins, still lets a consumer reconstruct the fully
// announced assistant reply.
func TestRunStreamCancelAfterAnnouncedCallsRebuilds(t *testing.T) {
	t.Parallel()

	call1 := llm.ToolUseBlock{Type: "tool_use", ID: "c1", Name: "t1", Input: json.RawMessage(`{}`)}
	call2 := llm.ToolUseBlock{Type: "tool_use", ID: "c2", Name: "t2", Input: json.RawMessage(`{}`)}
	registry := tool.NewRegistry()
	t1 := &countingTool{info: tool.ToolInfo{Name: "t1"}, result: tool.NewTextResult("r1")}
	t2 := &countingTool{info: tool.ToolInfo{Name: "t2"}, result: tool.NewTextResult("r2")}
	registry.MustRegister(t1)
	registry.MustRegister(t2)
	p := NewMockStreamingProvider([][]llm.Chunk{
		{
			llm.ToolCallStartChunk{Index: 0, ID: "c1", Name: "t1"},
			llm.ToolCallArgsChunk{Index: 0, ID: "c1", Delta: `{}`},
			llm.ToolCallStartChunk{Index: 1, ID: "c2", Name: "t2"},
			llm.ToolCallArgsChunk{Index: 1, ID: "c2", Delta: `{}`},
			llm.DoneChunk{FinishReason: "tool_calls"},
		},
	})
	ag := New(p, registry, WithLogger(discardLogger()))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	seq, err := ag.RunStream(ctx, "go")
	if err != nil {
		t.Fatalf("RunStream returned error: %v", err)
	}

	var events []AgentEvent
	var termErr error
	for ev, err := range seq {
		if err != nil {
			termErr = err
			break
		}
		events = append(events, ev)
		if tc, ok := ev.(ToolCallEvent); ok && tc.ID == "c2" {
			// Cancel only after the full round has been announced.
			cancel()
		}
	}
	if !errors.Is(termErr, context.Canceled) {
		t.Fatalf("terminal error = %v, want context.Canceled", termErr)
	}
	if t1.executions.Load() != 0 || t2.executions.Load() != 0 {
		t.Errorf("tool executions = %d/%d, want 0/0", t1.executions.Load(), t2.executions.Load())
	}

	seed := []llm.Message{llm.UserMessage("go")}
	fold := newEventFold(seed)
	for _, ev := range events {
		fold.apply(t, ev)
	}
	want := append(seed,
		llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{call1, call2}},
	)
	if got := fold.finishErr(); !reflect.DeepEqual(got, want) {
		t.Errorf("fold after cancellation\n got: %+v\nwant: %+v", got, want)
	}
}

// TestRunStreamCancelMidAnnouncementStillAnnouncesAll pins the narrower
// constraint: the announcement batch has no context check, so a cancel
// during delivery of the first announcement does not suppress the rest.
func TestRunStreamCancelMidAnnouncementStillAnnouncesAll(t *testing.T) {
	t.Parallel()

	call1 := llm.ToolUseBlock{Type: "tool_use", ID: "c1", Name: "t1", Input: json.RawMessage(`{}`)}
	call2 := llm.ToolUseBlock{Type: "tool_use", ID: "c2", Name: "t2", Input: json.RawMessage(`{}`)}
	registry := tool.NewRegistry()
	t1 := &countingTool{info: tool.ToolInfo{Name: "t1"}, result: tool.NewTextResult("r1")}
	registry.MustRegister(t1)
	p := NewMockStreamingProvider([][]llm.Chunk{
		{
			llm.ToolCallStartChunk{Index: 0, ID: "c1", Name: "t1"},
			llm.ToolCallArgsChunk{Index: 0, ID: "c1", Delta: `{}`},
			llm.ToolCallStartChunk{Index: 1, ID: "c2", Name: "t2"},
			llm.ToolCallArgsChunk{Index: 1, ID: "c2", Delta: `{}`},
			llm.DoneChunk{FinishReason: "tool_calls"},
		},
	})
	ag := New(p, registry, WithLogger(discardLogger()))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	seq, err := ag.RunStream(ctx, "go")
	if err != nil {
		t.Fatalf("RunStream returned error: %v", err)
	}

	var events []AgentEvent
	var announced []string
	var termErr error
	for ev, err := range seq {
		if err != nil {
			termErr = err
			break
		}
		events = append(events, ev)
		if tc, ok := ev.(ToolCallEvent); ok {
			announced = append(announced, tc.ID)
			if tc.ID == "c1" {
				cancel()
			}
		}
	}
	if len(announced) != 2 || announced[0] != "c1" || announced[1] != "c2" {
		t.Errorf("announced = %v, want [c1 c2] — the batch must not be split by cancellation", announced)
	}
	if !errors.Is(termErr, context.Canceled) {
		t.Fatalf("terminal error = %v, want context.Canceled", termErr)
	}
	if t1.executions.Load() != 0 {
		t.Errorf("t1 executions = %d, want 0", t1.executions.Load())
	}

	seed := []llm.Message{llm.UserMessage("go")}
	fold := newEventFold(seed)
	for _, ev := range events {
		fold.apply(t, ev)
	}
	want := append(seed,
		llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{call1, call2}},
	)
	if got := fold.finishErr(); !reflect.DeepEqual(got, want) {
		t.Errorf("fold after mid-announcement cancel\n got: %+v\nwant: %+v", got, want)
	}
}
