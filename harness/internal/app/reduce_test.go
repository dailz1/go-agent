package app

import (
	"testing"

	"github.com/dailz1/go-agent/agent"
	"github.com/dailz1/go-agent/harness/internal/session"
	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

// TestReducerSevenEventsValueAndPointer folds one full run through the
// reducer in value form and the same run in pointer form: both must produce
// identical view models, proving the sealed-union handling of all seven
// variants.
func TestReducerSevenEventsValueAndPointer(t *testing.T) {
	run := func(pointer bool) ViewModel {
		r := NewReducer("s-one")
		emit := func(ev agent.AgentEvent) {
			if pointer {
				switch e := ev.(type) {
				case agent.TextDeltaEvent:
					ev = &e
				case agent.ThinkingDeltaEvent:
					ev = &e
				case agent.ToolCallEvent:
					ev = &e
				case agent.ToolResultEvent:
					ev = &e
				case agent.RetryEvent:
					ev = &e
				case agent.CompactionEvent:
					ev = &e
				case agent.DoneEvent:
					ev = &e
				}
			}
			r.Apply(ev)
		}
		emit(agent.TextDeltaEvent{Text: "hello "})
		emit(agent.TextDeltaEvent{Text: "world"})
		emit(agent.ThinkingDeltaEvent{Text: "considering"})
		emit(agent.ToolCallEvent{ID: "tc1", Name: "read", Args: []byte(`{"path":"a.go"}`)})
		result := tool.NewTextResult("42 lines")
		emit(agent.ToolResultEvent{ID: "tc1", Name: "read", Result: result})
		emit(agent.RetryEvent{RetryInfo: agent.RetryInfo{
			Attempt: 2, MaxAttempts: 4, Reason: "rate limited (429)",
		}})
		history := []llm.Message{
			llm.UserMessage("do it"),
			llm.AssistantToolCallMessage(llm.ToolUseBlock{ID: "tc1", Name: "read"}),
			llm.ToolResultMessage("tc1", result),
			llm.AssistantMessage("hello world"),
		}
		emit(agent.DoneEvent{
			Message: llm.AssistantMessage("hello world"), History: history,
			ToolCalls: 1, ThreadID: "s-one", Usage: llm.Usage{InputTokens: 10, OutputTokens: 5},
			TotalUsage: llm.Usage{InputTokens: 30, OutputTokens: 9},
		})
		return r.View()
	}

	value, ptr := run(false), run(true)
	if value.Text != "hello world" || ptr.Text != "hello world" {
		t.Fatalf("text: value=%q pointer=%q", value.Text, ptr.Text)
	}
	if value.Thinking != "considering" || ptr.Thinking != "considering" {
		t.Fatalf("thinking: value=%q pointer=%q", value.Thinking, ptr.Thinking)
	}
	if len(value.Tools) != 1 || value.Tools[0].ID != "tc1" || value.Tools[0].Result == nil {
		t.Fatalf("tool cards = %+v", value.Tools)
	}
	if value.Tools[0].Result.Content != "42 lines" || value.Tools[0].Result.IsError {
		t.Fatalf("tool outcome = %+v", value.Tools[0].Result)
	}
	if len(value.Retries) != 1 || value.Retries[0].Attempt != 2 {
		t.Fatalf("retries = %+v", value.Retries)
	}
	if !value.Complete || value.Interrupted {
		t.Fatalf("completion flags = %+v", value)
	}
	if len(value.History) != 4 || value.ThreadID != "s-one" {
		t.Fatalf("history len=%d thread=%q", len(value.History), value.ThreadID)
	}
	if value.Usage.OutputTokens != 5 || value.TotalUsage.InputTokens != 30 {
		t.Fatalf("usage = %+v total=%+v", value.Usage, value.TotalUsage)
	}
	if value.Source != SourceLive || value.Cache != CacheNone {
		t.Fatalf("source = %v cache = %v", value.Source, value.Cache)
	}
}

// TestReducerDoneAlignsTextOnce proves Done replaces the tentative streamed
// text with the authoritative final message instead of appending it.
func TestReducerDoneAlignsTextOnce(t *testing.T) {
	r := NewReducer("t")
	r.Apply(agent.TextDeltaEvent{Text: "partial ans"})
	r.Apply(agent.TextDeltaEvent{Text: "wer"})
	r.Apply(agent.DoneEvent{Message: llm.AssistantMessage("partial answer"), History: nil})
	if got := r.View().Text; got != "partial answer" {
		t.Fatalf("text after Done = %q, want aligned replacement", got)
	}
	r.Apply(agent.DoneEvent{Message: llm.AssistantMessage("partial answer"), History: nil})
	if got := r.View().Text; got != "partial answer" {
		t.Fatalf("second Done appended: %q", got)
	}
}

// TestReducerInterruptKeepsObservationsWithoutDone proves an error end keeps
// tentative observations and marks the view incomplete; no Done appears.
func TestReducerInterruptKeepsObservationsWithoutDone(t *testing.T) {
	r := NewReducer("t")
	r.Apply(agent.TextDeltaEvent{Text: "working"})
	r.Apply(agent.ToolCallEvent{ID: "x", Name: "shell", Args: []byte(`{}`)})
	r.Interrupt()
	view := r.View()
	if view.Complete || !view.Interrupted {
		t.Fatalf("flags = complete:%v interrupted:%v", view.Complete, view.Interrupted)
	}
	if view.Text != "working" || len(view.Tools) != 1 || view.Tools[0].Result != nil {
		t.Fatalf("tentative observations lost: %+v", view)
	}
	if view.Tools[0].Result != nil {
		t.Fatal("unresolved tool call fabricated a result")
	}
}

// TestReducerCompactionExposesSignal proves the compaction notice carries
// the kernel's real strategy facts and swaps the effective history.
func TestReducerCompactionExposesSignal(t *testing.T) {
	r := NewReducer("t")
	effective := []llm.Message{llm.UserMessage("old task"), llm.AssistantMessage("folded tools")}
	r.Apply(agent.CompactionEvent{
		Strategies:    []string{"drop-oldest-tool-groups", "sliding-window"},
		DroppedGroups: 3, BeforeRunes: 9000, AfterRunes: 4200,
		History: effective,
	})
	view := r.View()
	if view.Compaction == nil {
		t.Fatal("compaction signal not exposed")
	}
	if view.Compaction.DroppedGroups != 3 || view.Compaction.BeforeRunes != 9000 ||
		view.Compaction.AfterRunes != 4200 {
		t.Fatalf("notice facts = %+v", view.Compaction)
	}
	if len(view.Compaction.Strategies) != 2 || view.Compaction.Strategies[0] != "drop-oldest-tool-groups" {
		t.Fatalf("strategies = %+v", view.Compaction.Strategies)
	}
	if len(view.History) != 2 {
		t.Fatalf("effective history not adopted: %d messages", len(view.History))
	}
}

// TestReducerReasoningAlignment proves reasoning display aligns with the
// authoritative history: readable summaries surface, items without a
// summary are marked unreadable, encrypted content never becomes text.
func TestReducerReasoningAlignment(t *testing.T) {
	history := []llm.Message{{
		Role: llm.RoleAssistant,
		Content: []llm.ContentBlock{
			llm.ReasoningItemBlock{Type: "reasoning_item", ID: "ri-1", Summary: []string{"checked imports"}},
			llm.ReasoningItemBlock{
				Type: "reasoning_item", ID: "ri-2",
				EncryptedContent: "opaque-ciphertext",
			},
			llm.ToolUseBlock{Type: "tool_use", ID: "tc1", Name: "read"},
		},
	}}
	r := NewReducer("t")
	r.Apply(agent.DoneEvent{Message: llm.AssistantMessage("done"), History: history})
	view := r.View()
	if len(view.Reasoning) != 2 {
		t.Fatalf("reasoning items = %+v", view.Reasoning)
	}
	if !view.Reasoning[0].Readable || view.Reasoning[0].ID != "ri-1" {
		t.Fatalf("readable item = %+v", view.Reasoning[0])
	}
	if view.Reasoning[1].Readable || len(view.Reasoning[1].Summary) != 0 {
		t.Fatalf("encrypted item must be unreadable with no summary: %+v", view.Reasoning[1])
	}
}

// TestFromCacheGradesHonesty proves the opened view distinguishes
// cache-sourced history from live events and grades staleness against the
// store head instead of pretending completeness.
func TestFromCacheGradesHonesty(t *testing.T) {
	history := []llm.Message{llm.UserMessage("one"), llm.AssistantMessage("two")}

	current := FromCache(session.Cache{Head: 5, History: history}, true, 5, "s-x")
	if current.Source != SourceCache || current.Cache != CacheCurrent {
		t.Fatalf("current = source:%v cache:%v", current.Source, current.Cache)
	}
	if !current.Complete || len(current.History) != 2 {
		t.Fatalf("current view = %+v", current)
	}

	stale := FromCache(session.Cache{Head: 3, History: history}, true, 5, "s-x")
	if stale.Cache != CacheStale || stale.Complete {
		t.Fatalf("stale = cache:%v complete:%v", stale.Cache, stale.Complete)
	}
	if len(stale.History) != 2 {
		t.Fatal("stale view discarded known history")
	}

	missing := FromCache(session.Cache{}, false, 5, "s-x")
	if missing.Cache != CacheNone || missing.History != nil || missing.Complete {
		t.Fatalf("missing = %+v", missing)
	}

	// A zero-head cache with content would be a saved-but-empty artifact:
	// treated as none rather than a fabricated complete view.
	empty := FromCache(session.Cache{Head: 0, History: nil}, true, 0, "s-x")
	if empty.Cache != CacheNone {
		t.Fatalf("empty = %+v", empty)
	}
}
