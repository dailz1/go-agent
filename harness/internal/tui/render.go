package tui

import (
	"fmt"
	"strings"

	"github.com/dailz1/go-agent/harness/internal/app"
)

// sectionKind labels what a rendered section is. The kinds are the render
// contract of §2.5: tests assert structure and flags, never prose.
type sectionKind int

const (
	sectionBanner     sectionKind = iota // cached/live provenance banner
	sectionText                          // streamed or final assistant text
	sectionThinking                      // live thinking delta (collapsed by default)
	sectionReasoning                     // authoritative reasoning items
	sectionTool                          // one tool card
	sectionRetry                         // one retry notice
	sectionCompaction                    // compaction notice
	sectionStatus                        // completion/interruption/resume markers
)

// chatSection is one rendered block with programmatic flags the tests and
// the flattener consume. Pending marks a tool card whose result event has
// not arrived — a requested call is never shown as executed.
type chatSection struct {
	Kind       sectionKind
	Cached     bool // belongs to the stored snapshot, not live output
	Collapsed  bool
	Pending    bool
	Error      bool
	Unreadable bool
	Lines      []string
}

type chatDoc struct{ Sections []chatSection }

// renderOpts shapes one render pass.
type renderOpts struct {
	Expanded bool // thinking/reasoning sections expanded
}

// toolPreviewCap bounds the result preview of one tool card in the chat
// scrollback; full content stays reachable through the tool's own outputs.
const toolPreviewCap = 50

// renderDoc folds a ViewModel into a chatDoc following the §2.5 display
// rules: tentative text and thinking are observations, tool cards stay
// pending until their result event, cached views are marked as snapshots
// with their cache grade, and no Done is ever synthesized.
func renderDoc(view app.ViewModel, opts renderOpts) chatDoc {
	doc := chatDoc{Sections: []chatSection{}}
	if view.Source == app.SourceCache {
		doc.Sections = append(doc.Sections, cachedBanner(view))
	}
	if view.Resuming {
		doc.Sections = append(doc.Sections, chatSection{
			Kind: sectionStatus, Lines: []string{
				"resuming interrupted task (non-streaming) — no deltas are fabricated",
			},
		})
	}
	if view.Thinking != "" {
		doc.Sections = append(doc.Sections, chatSection{
			Kind: sectionThinking, Collapsed: !opts.Expanded,
			Lines: splitLines(view.Thinking),
		})
	}
	if view.Text != "" {
		doc.Sections = append(doc.Sections, chatSection{
			Kind: sectionText, Lines: splitLines(view.Text),
		})
	}
	for _, card := range view.Tools {
		doc.Sections = append(doc.Sections, toolSection(card))
	}
	for _, retry := range view.Retries {
		doc.Sections = append(doc.Sections, chatSection{
			Kind: sectionRetry,
			Lines: []string{fmt.Sprintf("retry %d/%d after %s: %s",
				retry.Attempt, retry.MaxAttempts, retry.Delay, sanitize(retry.Reason))},
		})
	}
	if view.Compaction != nil {
		doc.Sections = append(doc.Sections, compactionSection(view.Compaction))
	}
	for _, item := range view.Reasoning {
		doc.Sections = append(doc.Sections, reasoningSection(item, opts.Expanded))
	}
	doc.Sections = append(doc.Sections, statusSections(view)...)
	return doc
}

// cachedBanner marks stored-snapshot provenance. Cached sections are
// visually distinct from live output: a banner carries the cache grade and
// head, and the section flags carry Cached=true for programmatic checks.
func cachedBanner(view app.ViewModel) chatSection {
	sec := chatSection{Kind: sectionBanner, Cached: true, Lines: []string{
		fmt.Sprintf("[saved session view] thread=%s snapshot-head=%d", view.ThreadID, view.Head),
	}}
	switch view.Cache {
	case app.CacheStale:
		sec.Lines = append(sec.Lines,
			"history view ends at the last snapshot; later records exist in the durable log")
	case app.CacheNone:
		sec.Lines = append(sec.Lines,
			"no saved display view; only durable facts are shown")
	default:
		sec.Lines = append(sec.Lines, "saved view matches the durable log head")
	}
	return sec
}

// toolSection renders one tool card. A card without a result is a request,
// not an execution; the result preview is bounded with an honest marker.
func toolSection(card app.ToolCard) chatSection {
	sec := chatSection{Kind: sectionTool, Lines: []string{
		fmt.Sprintf("tool %s (%s)", card.Name, shortID(card.ID)),
	}}
	args := sanitize(card.Args)
	if args != "" {
		sec.Lines = append(sec.Lines, previewLines(args, 8)...)
	}
	if card.Result == nil {
		sec.Pending = true
		sec.Lines = append(sec.Lines, "requested — awaiting result")
		return sec
	}
	sec.Error = card.Result.IsError
	content := sanitize(card.Result.Content)
	if content == "" {
		content = "(empty result)"
	}
	sec.Lines = append(sec.Lines, previewLines(content, toolPreviewCap)...)
	return sec
}

func compactionSection(notice *app.CompactionNotice) chatSection {
	return chatSection{Kind: sectionCompaction, Lines: []string{fmt.Sprintf(
		"context compacted: strategies=%s dropped-groups=%d runes=%d→%d",
		strings.Join(notice.Strategies, ","), notice.DroppedGroups,
		notice.BeforeRunes, notice.AfterRunes),
	}}
}

// reasoningSection renders one authoritative reasoning item. Collapsed by
// default; items without a readable summary say so instead of surfacing
// encrypted content as reasoning text.
func reasoningSection(item app.ReasoningItem, expanded bool) chatSection {
	sec := chatSection{Kind: sectionReasoning, Collapsed: !expanded, Lines: []string{
		"reasoning " + shortID(item.ID),
	}}
	if !item.Readable {
		sec.Unreadable = true
		sec.Lines = append(sec.Lines, "no readable summary for this reasoning item")
		return sec
	}
	if expanded {
		for _, line := range item.Summary {
			sec.Lines = append(sec.Lines, splitLines(sanitize(line))...)
		}
	} else {
		total := 0
		for _, line := range item.Summary {
			total += len([]rune(line))
		}
		sec.Lines = append(sec.Lines, fmt.Sprintf("collapsed — %d runes", total))
	}
	return sec
}

// statusSections closes the document with what the view claims: completion
// with usage, truncation, or an explicit interruption marker. An
// interrupted view never gains a completion line.
func statusSections(view app.ViewModel) []chatSection {
	out := []chatSection{}
	switch {
	case view.Interrupted:
		out = append(out, chatSection{Kind: sectionStatus, Lines: []string{
			"interrupted — observations above are unconfirmed; no completion was recorded",
		}})
	case view.Complete:
		line := fmt.Sprintf("done — in=%d out=%d reasoning=%d tokens (last run)",
			view.Usage.InputTokens, view.Usage.OutputTokens, view.Usage.ReasoningTokens)
		if view.TotalUsage.InputTokens > 0 || view.TotalUsage.OutputTokens > 0 {
			line += fmt.Sprintf("; total in=%d out=%d",
				view.TotalUsage.InputTokens, view.TotalUsage.OutputTokens)
		}
		out = append(out, chatSection{Kind: sectionStatus, Lines: []string{line}})
	}
	if view.Truncated {
		out = append(out, chatSection{Kind: sectionStatus, Lines: []string{
			"truncated: iteration limit reached; the task is NOT complete",
		}})
	}
	return out
}

// flatten renders the document for the scrollback viewport. Sections are
// separated by a blank line; collapsed sections render only their header
// lines (the first two) — the flags stay authoritative for tests.
func (d chatDoc) flatten() string {
	var b strings.Builder
	for i, sec := range d.Sections {
		if i > 0 {
			b.WriteByte('\n')
		}
		lines := sec.Lines
		if sec.Collapsed {
			lines = sec.Lines[:min(2, len(sec.Lines))]
		}
		for j, line := range lines {
			if j > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(expandTabs(line))
		}
	}
	return b.String()
}

// previewLines bounds a text block, appending an honest truncation marker
// naming how many lines were cut.
func previewLines(text string, cap int) []string {
	lines := splitLines(text)
	if len(lines) <= cap {
		return lines
	}
	kept := append([]string(nil), lines[:cap]...)
	return append(kept, fmt.Sprintf("… %d more lines not shown", len(lines)-cap))
}

func splitLines(text string) []string {
	return strings.Split(expandTabs(sanitize(text)), "\n")
}

func shortID(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12] + "…"
}
