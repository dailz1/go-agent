package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

// eventFold rebuilds history previews from stream events. DoneEvent.History is
// authoritative because persistence, fallback conversion, and future event
// additions can carry information not reconstructible from the event stream.
//
// Rebuild rules. A round announces ALL its ToolCallEvents before executing
// any, so contiguous announced calls identify one assistant reply: the fold
// flushes that reply (reasoning, then text, then calls) when the round's
// first ToolResultEvent arrives, and appends each result in event order. A
// terminal error keeps the reply only if calls were announced (the loop
// appends the assistant message before executing); pending deltas with no
// calls mean a pre-assembly failure, so they are discarded. CompactionEvent
// and DoneEvent snapshots replace the preview with independently copied
// authoritative history.
type eventFold struct {
	seed      []llm.Message
	history   []llm.Message
	reasoning strings.Builder
	text      strings.Builder
	calls     []llm.ToolUseBlock
}

func newEventFold(seed []llm.Message) *eventFold {
	return &eventFold{seed: seed}
}

func (f *eventFold) apply(t *testing.T, event AgentEvent) {
	t.Helper()
	switch e := event.(type) {
	case TextDeltaEvent:
		f.text.WriteString(e.Text)
	case ThinkingDeltaEvent:
		f.reasoning.WriteString(e.Text)
	case ToolCallEvent:
		f.calls = append(f.calls, llm.ToolUseBlock{
			Type:  "tool_use",
			ID:    e.ID,
			Name:  e.Name,
			Input: bytes.Clone(e.Args),
		})
	case ToolResultEvent:
		f.flushAssistant()
		f.history = append(f.history, llm.ToolResultMessage(e.ID, e.Result))
	case CompactionEvent:
		f.resetRound()
		f.seed = copyMessages(e.History)
		f.history = nil
	case RetryEvent:
		// Retries leave no trace in history.
	case DoneEvent:
		f.resetRound()
		f.seed = deepCopyMessages(e.History)
		f.history = nil
	default:
		t.Fatalf("eventFold: unexpected event type %T", event)
	}
}

// finishErr closes the fold at a terminal error. Announced calls prove the
// loop assembled and appended the assistant message (it appends before
// executing tools), so the reply survives with whatever results completed.
// No calls means the error hit before assembly — pre-assembly failures
// append nothing — so partial deltas are discarded.
func (f *eventFold) finishErr() []llm.Message {
	if len(f.calls) > 0 {
		f.flushAssistant()
	}
	return f.full()
}

func (f *eventFold) full() []llm.Message {
	out := make([]llm.Message, 0, len(f.seed)+len(f.history))
	out = append(out, f.seed...)
	out = append(out, f.history...)
	return out
}

// flushAssistant materializes the pending assistant reply — reasoning, text,
// and announced calls — into history. No-op when nothing is pending.
func (f *eventFold) flushAssistant() {
	var blocks []llm.ContentBlock
	if f.reasoning.Len() > 0 {
		blocks = append(blocks, llm.ReasoningBlock{Type: "reasoning", Content: f.reasoning.String()})
	}
	if f.text.Len() > 0 {
		blocks = append(blocks, llm.TextBlock{Type: "text", Text: f.text.String()})
	}
	for _, call := range f.calls {
		blocks = append(blocks, call)
	}
	if len(blocks) == 0 {
		return
	}
	f.history = append(f.history, llm.Message{Role: llm.RoleAssistant, Content: blocks})
	f.resetRound()
}

func (f *eventFold) resetRound() {
	f.reasoning.Reset()
	f.text.Reset()
	f.calls = nil
}

func TestEventFoldDoneSnapshotReplacesPreviewAndDeepCopies(t *testing.T) {
	fold := newEventFold([]llm.Message{llm.UserMessage("old")})
	fold.apply(t, TextDeltaEvent{Text: "preview"})
	history := []llm.Message{
		llm.UserMessage("go"),
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			llm.ToolUseBlock{Type: "tool_use", ID: "c1", Name: "echo", Input: json.RawMessage(`{"x":1}`)},
		}},
		llm.ToolResultMessage("c1", tool.NewErrorResult("not executed: the run reached its iteration limit")),
	}
	fold.apply(t, DoneEvent{History: history, Truncated: true})
	if !reflect.DeepEqual(fold.full(), history) {
		t.Fatalf("Done snapshot did not replace preview: %#v", fold.full())
	}
	history[1].Content[0].(llm.ToolUseBlock).Input[2] = 'z'
	got := fold.full()[1].Content[0].(llm.ToolUseBlock).Input
	if string(got) != `{"x":1}` {
		t.Errorf("fold retained DoneEvent mutable input: %q", got)
	}
}

// runAndFold streams a full run, folds every event, and returns the events,
// the DoneEvent, and the folded history for comparison.
func runAndFold(t *testing.T, ag *Agent, input string, seed []llm.Message) ([]AgentEvent, DoneEvent, []llm.Message) {
	t.Helper()
	seq, err := ag.RunStream(context.Background(), input)
	if err != nil {
		t.Fatalf("RunStream returned error: %v", err)
	}
	events, errs := collectEvents(t, seq, nil)
	if len(errs) != 0 {
		t.Fatalf("RunStream stream errors: %v", errs)
	}
	fold := newEventFold(seed)
	var done DoneEvent
	found := false
	for _, ev := range events {
		if d, ok := ev.(DoneEvent); ok {
			done, found = d, true
		}
		fold.apply(t, ev)
	}
	if !found {
		t.Fatal("RunStream emitted no DoneEvent")
	}
	folded := fold.full()
	return events, done, folded
}

func TestAgentEventFoldRebuildsHistory(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		setup func(t *testing.T) (ag *Agent, input string, seed []llm.Message, want []llm.Message, check func(t *testing.T, events []AgentEvent, done DoneEvent))
	}{
		{
			name: "consecutive tool-only rounds are distinguishable",
			setup: func(t *testing.T) (*Agent, string, []llm.Message, []llm.Message, func(t *testing.T, events []AgentEvent, done DoneEvent)) {
				call1 := llm.ToolUseBlock{Type: "tool_use", ID: "c1", Name: "t1", Input: json.RawMessage(`{}`)}
				call2 := llm.ToolUseBlock{Type: "tool_use", ID: "c2", Name: "t2", Input: json.RawMessage(`{}`)}
				registry := tool.NewRegistry()
				registry.MustRegister(&mockTool{info: tool.ToolInfo{Name: "t1"}, result: tool.NewTextResult("r1")})
				registry.MustRegister(&mockTool{info: tool.ToolInfo{Name: "t2"}, result: tool.NewTextResult("r2")})
				p := NewMockStreamingProvider([][]llm.Chunk{
					{
						llm.ToolCallStartChunk{Index: 0, ID: "c1", Name: "t1"},
						llm.ToolCallArgsChunk{Index: 0, ID: "c1", Delta: `{}`},
						llm.DoneChunk{FinishReason: "tool_calls"},
					},
					{
						llm.ToolCallStartChunk{Index: 0, ID: "c2", Name: "t2"},
						llm.ToolCallArgsChunk{Index: 0, ID: "c2", Delta: `{}`},
						llm.DoneChunk{FinishReason: "tool_calls"},
					},
					{
						llm.TextDeltaChunk{Text: "done"},
						llm.DoneChunk{FinishReason: "stop"},
					},
				})
				ag := New(p, registry, WithLogger(discardLogger()))
				seed := []llm.Message{llm.UserMessage("go")}
				want := append(seed,
					llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{call1}},
					llm.ToolResultMessage("c1", tool.NewTextResult("r1")),
					llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{call2}},
					llm.ToolResultMessage("c2", tool.NewTextResult("r2")),
					llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
						llm.TextBlock{Type: "text", Text: "done"},
					}},
				)
				return ag, "go", seed, want, nil
			},
		},
		{
			name: "text only round with system prompt",
			setup: func(t *testing.T) (*Agent, string, []llm.Message, []llm.Message, func(t *testing.T, events []AgentEvent, done DoneEvent)) {
				p := NewMockStreamingProvider([][]llm.Chunk{{
					llm.TextDeltaChunk{Text: "Hello"},
					llm.TextDeltaChunk{Text: "!"},
					llm.DoneChunk{FinishReason: "stop"},
				}})
				ag := New(p, tool.NewRegistry(), WithSystemPrompt("be brief"), WithLogger(discardLogger()))
				seed := []llm.Message{llm.SystemMessage("be brief"), llm.UserMessage("hi")}
				want := append(seed, llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
					llm.TextBlock{Type: "text", Text: "Hello!"},
				}})
				return ag, "hi", seed, want, nil
			},
		},
		{
			name: "reasoning deltas then text",
			setup: func(t *testing.T) (*Agent, string, []llm.Message, []llm.Message, func(t *testing.T, events []AgentEvent, done DoneEvent)) {
				p := NewMockStreamingProvider([][]llm.Chunk{{
					llm.ReasoningDeltaChunk{Text: "step1 "},
					llm.ReasoningDeltaChunk{Text: "step2 "},
					llm.TextDeltaChunk{Text: "Answer: 4"},
					llm.DoneChunk{FinishReason: "stop"},
				}})
				ag := New(p, tool.NewRegistry(), WithLogger(discardLogger()))
				seed := []llm.Message{llm.UserMessage("2+2")}
				want := append(seed, llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
					llm.ReasoningBlock{Type: "reasoning", Content: "step1 step2 "},
					llm.TextBlock{Type: "text", Text: "Answer: 4"},
				}})
				return ag, "2+2", seed, want, nil
			},
		},
		{
			name: "single tool call round then final text",
			setup: func(t *testing.T) (*Agent, string, []llm.Message, []llm.Message, func(t *testing.T, events []AgentEvent, done DoneEvent)) {
				call := llm.ToolUseBlock{Type: "tool_use", ID: "c1", Name: "echo", Input: json.RawMessage(`{}`)}
				registry := tool.NewRegistry()
				registry.MustRegister(&mockTool{info: tool.ToolInfo{Name: "echo"}, result: tool.NewTextResult("hi echo")})
				p := NewMockStreamingProvider([][]llm.Chunk{
					{
						llm.TextDeltaChunk{Text: "calling "},
						llm.ToolCallStartChunk{Index: 0, ID: "c1", Name: "echo"},
						llm.ToolCallArgsChunk{Index: 0, ID: "c1", Delta: `{}`},
						llm.DoneChunk{FinishReason: "tool_calls"},
					},
					{
						llm.TextDeltaChunk{Text: "done: hi echo"},
						llm.DoneChunk{FinishReason: "stop"},
					},
				})
				ag := New(p, registry, WithLogger(discardLogger()))
				seed := []llm.Message{llm.UserMessage("hi")}
				want := append(seed,
					llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
						llm.TextBlock{Type: "text", Text: "calling "},
						call,
					}},
					llm.ToolResultMessage("c1", tool.NewTextResult("hi echo")),
					llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
						llm.TextBlock{Type: "text", Text: "done: hi echo"},
					}},
				)
				return ag, "hi", seed, want, nil
			},
		},
		{
			name: "two announced calls keep round order across results",
			setup: func(t *testing.T) (*Agent, string, []llm.Message, []llm.Message, func(t *testing.T, events []AgentEvent, done DoneEvent)) {
				call1 := llm.ToolUseBlock{Type: "tool_use", ID: "c1", Name: "t1", Input: json.RawMessage(`{}`)}
				call2 := llm.ToolUseBlock{Type: "tool_use", ID: "c2", Name: "t2", Input: json.RawMessage(`{}`)}
				registry := tool.NewRegistry()
				registry.MustRegister(&mockTool{info: tool.ToolInfo{Name: "t1"}, result: tool.NewTextResult("r1")})
				registry.MustRegister(&mockTool{info: tool.ToolInfo{Name: "t2"}, result: tool.NewTextResult("r2")})
				p := NewMockStreamingProvider([][]llm.Chunk{
					{
						llm.TextDeltaChunk{Text: "two calls"},
						llm.ToolCallStartChunk{Index: 0, ID: "c1", Name: "t1"},
						llm.ToolCallArgsChunk{Index: 0, ID: "c1", Delta: `{}`},
						llm.ToolCallStartChunk{Index: 1, ID: "c2", Name: "t2"},
						llm.ToolCallArgsChunk{Index: 1, ID: "c2", Delta: `{}`},
						llm.DoneChunk{FinishReason: "tool_calls"},
					},
					{
						llm.TextDeltaChunk{Text: "ok"},
						llm.DoneChunk{FinishReason: "stop"},
					},
				})
				ag := New(p, registry, WithLogger(discardLogger()))
				seed := []llm.Message{llm.UserMessage("go")}
				want := append(seed,
					llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
						llm.TextBlock{Type: "text", Text: "two calls"},
						call1,
						call2,
					}},
					llm.ToolResultMessage("c1", tool.NewTextResult("r1")),
					llm.ToolResultMessage("c2", tool.NewTextResult("r2")),
					llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
						llm.TextBlock{Type: "text", Text: "ok"},
					}},
				)
				return ag, "go", seed, want, nil
			},
		},
		{
			name: "approval rejection feeds foldable error result",
			setup: func(t *testing.T) (*Agent, string, []llm.Message, []llm.Message, func(t *testing.T, events []AgentEvent, done DoneEvent)) {
				registry := tool.NewRegistry()
				registry.MustRegister(&mockTool{info: tool.ToolInfo{Name: "guarded", RequiresApproval: true}, result: tool.NewTextResult("secret")})
				p := NewMockStreamingProvider([][]llm.Chunk{
					{
						llm.ToolCallStartChunk{Index: 0, ID: "g1", Name: "guarded"},
						llm.ToolCallArgsChunk{Index: 0, ID: "g1", Delta: `{}`},
						llm.DoneChunk{FinishReason: "tool_calls"},
					},
					{
						llm.TextDeltaChunk{Text: "fine"},
						llm.DoneChunk{FinishReason: "stop"},
					},
				})
				ag := New(p, registry, WithApprovalFn(func(tool.ToolInfo, json.RawMessage) bool { return false }), WithLogger(discardLogger()))
				seed := []llm.Message{llm.UserMessage("go")}
				check := func(t *testing.T, _ []AgentEvent, done DoneEvent) {
					t.Helper()
					resultMsg := done.History[2]
					block, ok := resultMsg.Content[0].(llm.ToolResultBlock)
					if !ok || !block.IsError {
						t.Errorf("rejection result = %#v, want error ToolResultBlock", resultMsg.Content[0])
					}
				}
				return ag, "go", seed, nil, check
			},
		},
		{
			name: "unknown tool soft error feeds foldable error result",
			setup: func(t *testing.T) (*Agent, string, []llm.Message, []llm.Message, func(t *testing.T, events []AgentEvent, done DoneEvent)) {
				p := NewMockStreamingProvider([][]llm.Chunk{
					{
						llm.ToolCallStartChunk{Index: 0, ID: "g1", Name: "ghost"},
						llm.ToolCallArgsChunk{Index: 0, ID: "g1", Delta: `{}`},
						llm.DoneChunk{FinishReason: "tool_calls"},
					},
					{
						llm.TextDeltaChunk{Text: "recovered"},
						llm.DoneChunk{FinishReason: "stop"},
					},
				})
				ag := New(p, tool.NewRegistry(), WithLogger(discardLogger()))
				seed := []llm.Message{llm.UserMessage("go")}
				check := func(t *testing.T, _ []AgentEvent, done DoneEvent) {
					t.Helper()
					block, ok := done.History[2].Content[0].(llm.ToolResultBlock)
					if !ok || !block.IsError {
						t.Errorf("unknown-tool result = %#v, want error ToolResultBlock", done.History[2].Content[0])
					}
				}
				return ag, "go", seed, nil, check
			},
		},
		{
			name: "oversized tool result folds as the truncated model view",
			setup: func(t *testing.T) (*Agent, string, []llm.Message, []llm.Message, func(t *testing.T, events []AgentEvent, done DoneEvent)) {
				registry := tool.NewRegistry()
				registry.MustRegister(&mockTool{info: tool.ToolInfo{Name: "big"}, result: tool.NewTextResult(strings.Repeat("x", 1000))})
				p := NewMockStreamingProvider([][]llm.Chunk{
					{
						llm.ToolCallStartChunk{Index: 0, ID: "c1", Name: "big"},
						llm.ToolCallArgsChunk{Index: 0, ID: "c1", Delta: `{}`},
						llm.DoneChunk{FinishReason: "tool_calls"},
					},
					{
						llm.TextDeltaChunk{Text: "short"},
						llm.DoneChunk{FinishReason: "stop"},
					},
				})
				ag := New(p, registry, WithContextWindowTokens(1000), WithLogger(discardLogger()))
				seed := []llm.Message{llm.UserMessage("go")}
				check := func(t *testing.T, events []AgentEvent, done DoneEvent) {
					t.Helper()
					var seen int
					for _, ev := range events {
						if tr, ok := ev.(ToolResultEvent); ok {
							seen++
							if got := utf8.RuneCountInString(tr.Result.Content); got != 300 {
								t.Errorf("truncated event content = %d runes, want 300", got)
							}
						}
					}
					if seen != 1 {
						t.Fatalf("ToolResultEvent count = %d, want 1", seen)
					}
					block := done.History[2].Content[0].(llm.ToolResultBlock)
					if got := utf8.RuneCountInString(block.Content); got != 300 {
						t.Errorf("history result content = %d runes, want 300", got)
					}
				}
				return ag, "go", seed, nil, check
			},
		},
		{
			name: "finish-only round after a tool round has an empty final message",
			setup: func(t *testing.T) (*Agent, string, []llm.Message, []llm.Message, func(t *testing.T, events []AgentEvent, done DoneEvent)) {
				call := llm.ToolUseBlock{Type: "tool_use", ID: "c1", Name: "echo", Input: json.RawMessage(`{}`)}
				registry := tool.NewRegistry()
				registry.MustRegister(&mockTool{info: tool.ToolInfo{Name: "echo"}, result: tool.NewTextResult("hi")})
				p := NewMockStreamingProvider([][]llm.Chunk{
					{
						llm.TextDeltaChunk{Text: "working "},
						llm.ToolCallStartChunk{Index: 0, ID: "c1", Name: "echo"},
						llm.ToolCallArgsChunk{Index: 0, ID: "c1", Delta: `{}`},
						llm.DoneChunk{FinishReason: "tool_calls"},
					},
					{
						llm.DoneChunk{FinishReason: "stop"},
					},
				})
				ag := New(p, registry, WithLogger(discardLogger()))
				seed := []llm.Message{llm.UserMessage("go")}
				want := append(seed,
					llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
						llm.TextBlock{Type: "text", Text: "working "},
						call,
					}},
					llm.ToolResultMessage("c1", tool.NewTextResult("hi")),
					llm.Message{Role: llm.RoleAssistant},
				)
				return ag, "go", seed, want, nil
			},
		},
		{
			name: "truncated final round reconciles DoneEvent history",
			setup: func(t *testing.T) (*Agent, string, []llm.Message, []llm.Message, func(t *testing.T, events []AgentEvent, done DoneEvent)) {
				registry := tool.NewRegistry()
				registry.MustRegister(&mockTool{info: tool.ToolInfo{Name: "echo"}, result: tool.NewTextResult("hi")})
				p := NewMockStreamingProvider([][]llm.Chunk{
					{
						llm.TextDeltaChunk{Text: "partial thought "},
						llm.ToolCallStartChunk{Index: 0, ID: "c1", Name: "echo"},
						llm.ToolCallArgsChunk{Index: 0, ID: "c1", Delta: `{}`},
						llm.DoneChunk{FinishReason: "tool_calls"},
					},
				})
				ag := New(p, registry, WithMaxIter(1), WithLogger(discardLogger()))
				seed := []llm.Message{llm.UserMessage("go")}
				want := append(seed, llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
					llm.TextBlock{Type: "text", Text: "partial thought "},
					llm.ToolUseBlock{Type: "tool_use", ID: "c1", Name: "echo", Input: json.RawMessage(`{}`)},
				}}, llm.ToolResultMessage("c1", tool.NewErrorResult("not executed: the run reached its iteration limit")))
				check := func(t *testing.T, _ []AgentEvent, done DoneEvent) {
					t.Helper()
					if !done.Truncated {
						t.Error("DoneEvent.Truncated = false, want true")
					}
				}
				return ag, "go", seed, want, check
			},
		},
		{
			name: "maxIter stop pairs terminal call with result",
			setup: func(t *testing.T) (*Agent, string, []llm.Message, []llm.Message, func(t *testing.T, events []AgentEvent, done DoneEvent)) {
				call := llm.ToolUseBlock{Type: "tool_use", ID: "c1", Name: "echo", Input: json.RawMessage(`{}`)}
				registry := tool.NewRegistry()
				registry.MustRegister(&mockTool{info: tool.ToolInfo{Name: "echo"}, result: tool.NewTextResult("hi")})
				p := NewMockStreamingProvider([][]llm.Chunk{
					{
						llm.ToolCallStartChunk{Index: 0, ID: "c1", Name: "echo"},
						llm.ToolCallArgsChunk{Index: 0, ID: "c1", Delta: `{}`},
						llm.DoneChunk{FinishReason: "tool_calls"},
					},
				})
				ag := New(p, registry, WithMaxIter(1), WithLogger(discardLogger()))
				seed := []llm.Message{llm.UserMessage("go")}
				want := append(seed,
					llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{call}},
					llm.ToolResultMessage("c1", tool.NewErrorResult("not executed: the run reached its iteration limit")),
				)
				check := func(t *testing.T, _ []AgentEvent, done DoneEvent) {
					t.Helper()
					if !done.Truncated {
						t.Error("DoneEvent.Truncated = false, want true")
					}
				}
				return ag, "go", seed, want, check
			},
		},
		{
			name: "compaction snapshot resyncs the fold",
			setup: func(t *testing.T) (*Agent, string, []llm.Message, []llm.Message, func(t *testing.T, events []AgentEvent, done DoneEvent)) {
				registry := tool.NewRegistry()
				registry.MustRegister(&mockTool{info: tool.ToolInfo{Name: "echo"}, result: tool.NewTextResult(strings.Repeat("z", 150))})
				p := NewMockStreamingProvider([][]llm.Chunk{
					{
						llm.TextDeltaChunk{Text: strings.Repeat("y", 150)},
						llm.ToolCallStartChunk{Index: 0, ID: "c1", Name: "echo"},
						llm.ToolCallArgsChunk{Index: 0, ID: "c1", Delta: `{}`},
						llm.DoneChunk{FinishReason: "tool_calls"},
					},
					{
						llm.TextDeltaChunk{Text: "final"},
						llm.DoneChunk{FinishReason: "stop"},
					},
				})
				compactor := CompactFunc(func(_ context.Context, history []llm.Message, _ CompactionBudget) (CompactionResult, error) {
					return CompactionResult{History: history[:1], Strategies: []string{"stub-keep-first"}, DroppedGroups: 1, Changed: true}, nil
				})
				ag := New(p, registry, WithContextWindowTokens(200), WithCompactor(compactor), WithLogger(discardLogger()))
				seed := []llm.Message{llm.UserMessage("go")}
				want := append(seed, llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
					llm.TextBlock{Type: "text", Text: "final"},
				}})
				check := func(t *testing.T, events []AgentEvent, _ DoneEvent) {
					t.Helper()
					var compactions []CompactionEvent
					for _, ev := range events {
						if c, ok := ev.(CompactionEvent); ok {
							compactions = append(compactions, c)
						}
					}
					if len(compactions) != 1 {
						t.Fatalf("CompactionEvent count = %d, want 1", len(compactions))
					}
					if compactions[0].DroppedGroups != 1 {
						t.Errorf("DroppedGroups = %d, want 1", compactions[0].DroppedGroups)
					}
				}
				return ag, "go", seed, want, check
			},
		},
		{
			name: "retry events leave no trace in history",
			setup: func(t *testing.T) (*Agent, string, []llm.Message, []llm.Message, func(t *testing.T, events []AgentEvent, done DoneEvent)) {
				p := NewRetryableStreamingMockProvider(
					&llm.APIError{StatusCode: 429, Body: "rate limited"},
					1,
					[][]llm.Chunk{{
						llm.TextDeltaChunk{Text: "ok"},
						llm.DoneChunk{FinishReason: "stop"},
					}},
				)
				ag := New(p, tool.NewRegistry(), WithRetryConfig(AgentRetryConfig{
					MaxRetries: 1,
					BaseDelay:  time.Millisecond,
					MaxDelay:   time.Millisecond,
				}), WithLogger(discardLogger()))
				seed := []llm.Message{llm.UserMessage("go")}
				want := append(seed, llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
					llm.TextBlock{Type: "text", Text: "ok"},
				}})
				check := func(t *testing.T, events []AgentEvent, _ DoneEvent) {
					t.Helper()
					var retries int
					for _, ev := range events {
						if _, ok := ev.(RetryEvent); ok {
							retries++
						}
					}
					if retries != 1 {
						t.Errorf("RetryEvent count = %d, want 1", retries)
					}
				}
				return ag, "go", seed, want, check
			},
		},
		{
			name: "non-streaming fallback synthesizes foldable events",
			setup: func(t *testing.T) (*Agent, string, []llm.Message, []llm.Message, func(t *testing.T, events []AgentEvent, done DoneEvent)) {
				call := llm.ToolUseBlock{Type: "tool_use", ID: "c1", Name: "echo", Input: json.RawMessage(`{}`)}
				registry := tool.NewRegistry()
				registry.MustRegister(&mockTool{info: tool.ToolInfo{Name: "echo"}, result: tool.NewTextResult("hi echo")})
				toolRound := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
					llm.TextBlock{Type: "text", Text: "calling "},
					call,
				}}
				p := NewMockProvider(
					MsgResponse(toolRound),
					MsgResponse(llm.AssistantMessage("done: hi echo")),
				)
				ag := New(p, registry, WithLogger(discardLogger()))
				seed := []llm.Message{llm.UserMessage("hi")}
				want := append(seed,
					toolRound,
					llm.ToolResultMessage("c1", tool.NewTextResult("hi echo")),
					llm.AssistantMessage("done: hi echo"),
				)
				return ag, "hi", seed, want, nil
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ag, input, seed, want, check := tt.setup(t)
			events, done, folded := runAndFold(t, ag, input, seed)
			if !reflect.DeepEqual(folded, done.History) {
				t.Errorf("fold(seed + events) != DoneEvent.History\n got: %+v\nwant: %+v", folded, done.History)
			}
			if want != nil && !reflect.DeepEqual(done.History, want) {
				t.Errorf("DoneEvent.History != expected\n got: %+v\nwant: %+v", done.History, want)
			}
			if check != nil {
				check(t, events, done)
			}
		})
	}
}
