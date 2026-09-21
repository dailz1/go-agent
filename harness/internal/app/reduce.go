package app

import (
	"time"

	"github.com/dailz1/go-agent/agent"
	"github.com/dailz1/go-agent/harness/internal/session"
	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

// ViewSource distinguishes what the view model was built from. Live events
// are observations of the current attempt; a cache view is a stored
// snapshot that may lag the durable log. Stage E renders the difference
// honestly instead of blending them.
type ViewSource int

const (
	SourceLive ViewSource = iota
	SourceCache
)

// CacheStatus grades a cache view against the store head it was opened with.
type CacheStatus int

const (
	CacheNone    CacheStatus = iota // no cache exists; only durable facts shown
	CacheStale                      // cache head trails the store head
	CacheCurrent                    // cache head matches the store head
)

// CompactionNotice is the signal exposed for the UI layer when the kernel
// compacts: what strategy ran and how much context shrank. It reports rune
// estimates, never invented token percentages.
type CompactionNotice struct {
	Strategies    []string
	DroppedGroups int
	BeforeRunes   int
	AfterRunes    int
	At            time.Time
}

// ToolCard tracks one announced tool call. Requested means the model asked;
// it is not execution, and a result is not a durable round commit.
type ToolCard struct {
	ID     string
	Name   string
	Args   string
	Result *ToolOutcome
}

// ToolOutcome is the recorded result of one tool call.
type ToolOutcome struct {
	IsError bool
	Content string
}

// RetryNotice reports one real kernel retry attempt.
type RetryNotice struct {
	Attempt     int
	MaxAttempts int
	Delay       time.Duration
	Reason      string
}

// ReasoningItem is one reasoning entry aligned with the authoritative
// history. Readable is false when the item carries no readable summary —
// encrypted content is never surfaced as if it were reasoning text.
type ReasoningItem struct {
	ID       string
	Summary  []string
	Readable bool
}

// ViewModel is the render contract for stage E. Tentative observations
// (text, thinking, tool cards) describe the current attempt; History is the
// authoritative snapshot once Done or compaction delivered one. Interrupted
// keeps the tentative observations and never fabricates a Done.
type ViewModel struct {
	Source      ViewSource
	Cache       CacheStatus
	Resuming    bool // non-streaming ResumeThread in flight (D3)
	Text        string
	Thinking    string
	Tools       []ToolCard
	Retries     []RetryNotice
	Compaction  *CompactionNotice
	History     []llm.Message
	Reasoning   []ReasoningItem
	Head        int64
	Usage       llm.Usage
	TotalUsage  llm.Usage
	Truncated   bool
	Complete    bool
	Interrupted bool
	ThreadID    string
}

// Reducer folds one AgentEvent stream into a ViewModel. It is not a kernel
// replay: it never reconstructs events it did not observe, and reasoning
// items without a readable summary are marked unreadable instead of guessed.
type Reducer struct {
	view ViewModel
}

// NewReducer starts a live view for one run.
func NewReducer(threadID string) *Reducer {
	return &Reducer{view: ViewModel{Source: SourceLive, ThreadID: threadID}}
}

// Apply folds one event. All seven variants are handled in both value and
// pointer form; an unknown variant is ignored rather than guessed at.
func (r *Reducer) Apply(ev agent.AgentEvent) {
	switch e := ev.(type) {
	case agent.TextDeltaEvent:
		r.view.Text += e.Text
	case *agent.TextDeltaEvent:
		r.view.Text += e.Text
	case agent.ThinkingDeltaEvent:
		r.view.Thinking += e.Text
	case *agent.ThinkingDeltaEvent:
		r.view.Thinking += e.Text
	case agent.ToolCallEvent:
		r.appendCall(e.ID, e.Name, string(e.Args))
	case *agent.ToolCallEvent:
		r.appendCall(e.ID, e.Name, string(e.Args))
	case agent.ToolResultEvent:
		r.setResult(e.ID, e.Result)
	case *agent.ToolResultEvent:
		r.setResult(e.ID, e.Result)
	case agent.RetryEvent:
		r.view.Retries = append(r.view.Retries, RetryNotice{
			Attempt: e.Attempt, MaxAttempts: e.MaxAttempts,
			Delay: e.Delay, Reason: e.Reason,
		})
	case agent.CompactionEvent:
		r.compact(e.Strategies, e.DroppedGroups, e.BeforeRunes, e.AfterRunes, e.History)
	case *agent.CompactionEvent:
		r.compact(e.Strategies, e.DroppedGroups, e.BeforeRunes, e.AfterRunes, e.History)
	case agent.DoneEvent:
		r.finish(e.Message, e.History, e.ThreadID, e.Truncated, e.Usage, e.TotalUsage)
	case *agent.DoneEvent:
		r.finish(e.Message, e.History, e.ThreadID, e.Truncated, e.Usage, e.TotalUsage)
	}
}

// Interrupt records an error or Done-less end: tentative observations stay,
// the view is explicitly incomplete, and no Done is synthesized.
func (r *Reducer) Interrupt() { r.view.Interrupted = true }

// View returns a snapshot safe to hand to a renderer.
func (r *Reducer) View() ViewModel {
	out := r.view
	out.Tools = append([]ToolCard(nil), r.view.Tools...)
	out.Retries = append([]RetryNotice(nil), r.view.Retries...)
	out.Reasoning = append([]ReasoningItem(nil), r.view.Reasoning...)
	out.History = append([]llm.Message(nil), r.view.History...)
	if r.view.Compaction != nil {
		notice := *r.view.Compaction
		notice.Strategies = append([]string(nil), r.view.Compaction.Strategies...)
		out.Compaction = &notice
	}
	return out
}

// SetResuming marks the D3 non-streaming resume phase.
func (r *Reducer) SetResuming(on bool) { r.view.Resuming = on }

func (r *Reducer) appendCall(id, name, args string) {
	r.view.Tools = append(r.view.Tools, ToolCard{ID: id, Name: name, Args: args})
}

func (r *Reducer) setResult(id string, result *tool.ToolResult) {
	if result == nil {
		return
	}
	outcome := ToolOutcome{IsError: result.IsError(), Content: result.Content}
	for i := range r.view.Tools {
		if r.view.Tools[i].ID == id {
			r.view.Tools[i].Result = &outcome
			return
		}
	}
	card := ToolCard{ID: id, Result: &outcome}
	r.view.Tools = append(r.view.Tools, card)
}

func (r *Reducer) compact(strategies []string, dropped, before, after int, history []llm.Message) {
	r.view.Compaction = &CompactionNotice{
		Strategies:    append([]string(nil), strategies...),
		DroppedGroups: dropped, BeforeRunes: before, AfterRunes: after,
		At: time.Now(),
	}
	// The post-compaction history is the effective model context. It is a
	// legitimate cache anchor, but the controller owns head capture.
	r.view.History = history
	r.view.Reasoning = reasoningFromHistory(history)
}

func (r *Reducer) finish(msg llm.Message, history []llm.Message, threadID string, truncated bool, usage, total llm.Usage) {
	r.view.Complete = true
	r.view.Interrupted = false
	r.view.History = history
	r.view.Reasoning = reasoningFromHistory(history)
	r.view.Truncated = truncated
	r.view.Usage = usage
	r.view.TotalUsage = total
	if threadID != "" {
		r.view.ThreadID = threadID
	}
	// Alignment, not accumulation: the authoritative final message replaces
	// the tentative streamed text exactly once.
	r.view.Text = messageText(msg)
}

// CompleteWith adopts a non-streaming RunResult (ResumeThread) as the
// authoritative view. It is the D3 path: no deltas existed, so nothing is
// invented — the result's history is the only authority.
func (r *Reducer) CompleteWith(result *agent.RunResult) {
	r.finish(result.Message, result.History, result.ThreadID, result.Truncated, result.Usage, result.TotalUsage)
}

func messageText(msg llm.Message) string {
	text := ""
	for _, block := range msg.Content {
		if tb, ok := block.(llm.TextBlock); ok {
			text += tb.Text
		}
	}
	return text
}

// reasoningFromHistory aligns reasoning display with the authoritative
// snapshot. ReasoningItemBlock entries surface their summary when one
// exists and are marked unreadable otherwise; Chat ReasoningBlock content
// is readable by construction. Encrypted content never becomes display text.
func reasoningFromHistory(history []llm.Message) []ReasoningItem {
	items := []ReasoningItem{}
	for _, msg := range history {
		for _, block := range msg.Content {
			switch b := block.(type) {
			case llm.ReasoningItemBlock:
				items = append(items, ReasoningItem{
					ID: b.ID, Summary: b.Summary, Readable: len(b.Summary) > 0,
				})
			case llm.ReasoningBlock:
				items = append(items, ReasoningItem{
					Summary: []string{b.Content}, Readable: b.Content != "",
				})
			}
		}
	}
	return items
}

// FromCache builds the opened-session view from a stored display cache,
// graded against the store head. A missing or trailing cache is represented
// honestly: the view says what is known and that later records exist.
func FromCache(cache session.Cache, cacheOK bool, storeHead int64, threadID string) ViewModel {
	view := ViewModel{
		Source: SourceCache, ThreadID: threadID,
		History: cache.History, Head: cache.Head,
	}
	view.Reasoning = reasoningFromHistory(cache.History)
	switch {
	case !cacheOK || cache.Head == 0 && len(cache.History) == 0:
		view.Cache = CacheNone
		view.History = nil
		view.Reasoning = nil
	case cache.Head < storeHead:
		view.Cache = CacheStale
	default:
		view.Cache = CacheCurrent
		view.Complete = true
	}
	return view
}
