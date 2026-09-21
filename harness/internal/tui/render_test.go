package tui

import (
	"strings"
	"testing"

	"github.com/dailz1/go-agent/harness/internal/app"
)

func kinds(doc chatDoc) []sectionKind {
	out := make([]sectionKind, 0, len(doc.Sections))
	for _, sec := range doc.Sections {
		out = append(out, sec.Kind)
	}
	return out
}

func hasKind(doc chatDoc, kind sectionKind) bool {
	for _, sec := range doc.Sections {
		if sec.Kind == kind {
			return true
		}
	}
	return false
}

func findKind(t *testing.T, doc chatDoc, kind sectionKind) chatSection {
	t.Helper()
	for _, sec := range doc.Sections {
		if sec.Kind == kind {
			return sec
		}
	}
	t.Fatalf("no %v section in doc: %v", kind, kinds(doc))
	return chatSection{}
}

// TestRenderDocSectionOrder pins the structural contract of §2.5: the
// provenance banner comes first when the view is cached, thinking precedes
// text, tool cards follow, and status closes the document. Only kinds and
// flags are asserted — never prose.
func TestRenderDocSectionOrder(t *testing.T) {
	view := app.ViewModel{
		Source:   app.SourceLive,
		Text:     "body text",
		Thinking: "thinking text",
		Tools: []app.ToolCard{
			{ID: "t1", Name: "read", Args: `{"path":"a.go"}`},
			{ID: "t2", Name: "edit", Args: `{"path":"b.go"}`, Result: &app.ToolOutcome{Content: "ok"}},
		},
		Retries:    []app.RetryNotice{{Attempt: 1, MaxAttempts: 3, Reason: "net"}},
		Compaction: &app.CompactionNotice{Strategies: []string{"s1"}, DroppedGroups: 2, BeforeRunes: 100, AfterRunes: 40},
		Reasoning:  []app.ReasoningItem{{ID: "r1", Summary: []string{"sum"}, Readable: true}},
		Complete:   true,
	}
	doc := renderDoc(view, renderOpts{})
	want := []sectionKind{
		sectionThinking, sectionText, sectionTool, sectionTool,
		sectionRetry, sectionCompaction, sectionReasoning, sectionStatus,
	}
	got := kinds(doc)
	if len(got) != len(want) {
		t.Fatalf("section count = %d (%v), want %d (%v)", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("section[%d] = %v, want %v (all: %v)", i, got[i], want[i], got)
		}
	}
	if hasKind(doc, sectionBanner) {
		t.Fatal("live view must not carry a cached banner")
	}
}

// A cached view opens with a banner section flagged Cached; a live view
// never has one. The flag is the programmatic marker, not the banner text.
func TestRenderDocCachedVsLiveMarker(t *testing.T) {
	cached := renderDoc(app.ViewModel{Source: app.SourceCache, ThreadID: "th", Head: 7, Cache: app.CacheStale}, renderOpts{})
	if got := findKind(t, cached, sectionBanner); !got.Cached {
		t.Fatal("banner section must carry the Cached flag")
	}
	for _, sec := range cached.Sections {
		if sec.Kind != sectionBanner && sec.Cached {
			t.Fatalf("non-banner section %v must not be flagged Cached", sec.Kind)
		}
	}
	live := renderDoc(app.ViewModel{Source: app.SourceLive, Text: "x"}, renderOpts{})
	if hasKind(live, sectionBanner) {
		t.Fatal("live view must not render a banner")
	}
}

// The three cache grades must all surface the banner; grade text is prose
// and deliberately not pinned.
func TestRenderDocCacheGradesHaveBanner(t *testing.T) {
	for _, grade := range []app.CacheStatus{app.CacheNone, app.CacheStale, app.CacheCurrent} {
		doc := renderDoc(app.ViewModel{Source: app.SourceCache, Cache: grade}, renderOpts{})
		if !hasKind(doc, sectionBanner) {
			t.Fatalf("cache grade %d: no banner section", grade)
		}
	}
}

// A requested tool call is an observation, not an execution: the card
// stays Pending until its result event arrives, then flips to settled
// with the error flag reflecting the outcome.
func TestToolCardPendingUntilResult(t *testing.T) {
	doc := renderDoc(app.ViewModel{Tools: []app.ToolCard{
		{ID: "t1", Name: "edit", Args: "{}"},
		{ID: "t2", Name: "shell", Args: "{}", Result: &app.ToolOutcome{Content: "out"}},
		{ID: "t3", Name: "read", Args: "{}", Result: &app.ToolOutcome{IsError: true, Content: "boom"}},
	}}, renderOpts{})
	sections := doc.Sections
	if len(sections) != 3 {
		t.Fatalf("got %d tool sections, want 3", len(sections))
	}
	if !sections[0].Pending {
		t.Fatal("tool without result must be Pending")
	}
	if sections[1].Pending || sections[1].Error {
		t.Fatal("tool with clean result must be settled and non-error")
	}
	if sections[2].Pending || !sections[2].Error {
		t.Fatal("error result must be settled with the error flag")
	}
}

// A tool result over the preview cap is cut with the count of dropped
// lines appended — the cap is a machine value, not prose.
func TestToolResultPreviewCap(t *testing.T) {
	lines := make([]string, toolPreviewCap+20)
	for i := range lines {
		lines[i] = "line"
	}
	doc := renderDoc(app.ViewModel{Tools: []app.ToolCard{
		{ID: "t", Name: "shell", Args: "{}", Result: &app.ToolOutcome{Content: strings.Join(lines, "\n")}},
	}}, renderOpts{})
	sec := doc.Sections[0]
	// header + args + capped preview + marker
	if got := len(sec.Lines); got < toolPreviewCap {
		t.Fatalf("preview kept %d lines, want at least the cap %d", got, toolPreviewCap)
	}
	if got := len(sec.Lines); got > toolPreviewCap+4 {
		t.Fatalf("preview kept %d lines, cap is %d", got, toolPreviewCap)
	}
}

// Thinking and reasoning sections are collapsed by default and expandable;
// an unreadable reasoning item is flagged instead of showing ciphertext.
func TestReasoningCollapsedByDefault(t *testing.T) {
	doc := renderDoc(app.ViewModel{
		Thinking:  "chain",
		Reasoning: []app.ReasoningItem{{ID: "r1", Summary: []string{"summary"}, Readable: true}},
	}, renderOpts{})
	thinking := findKind(t, doc, sectionThinking)
	if !thinking.Collapsed {
		t.Fatal("thinking must be collapsed by default")
	}
	reasoning := findKind(t, doc, sectionReasoning)
	if !reasoning.Collapsed {
		t.Fatal("reasoning must be collapsed by default")
	}

	expanded := renderDoc(app.ViewModel{
		Reasoning: []app.ReasoningItem{{ID: "r1", Summary: []string{"summary"}, Readable: true}},
	}, renderOpts{Expanded: true})
	reasoning = findKind(t, expanded, sectionReasoning)
	if reasoning.Collapsed || reasoning.Unreadable {
		t.Fatal("expanded readable reasoning must be open and readable")
	}

	sealed := renderDoc(app.ViewModel{
		Reasoning: []app.ReasoningItem{{ID: "r2", Readable: false}},
	}, renderOpts{Expanded: true})
	reasoning = findKind(t, sealed, sectionReasoning)
	if !reasoning.Unreadable {
		t.Fatal("reasoning without a summary must be flagged unreadable")
	}
}

// Interrupted views keep their observations and never gain a completion
// status; truncated completions carry the truncation marker; the resume
// path opens with a status section saying the phase is non-streaming.
func TestStatusSectionsHonesty(t *testing.T) {
	interrupted := renderDoc(app.ViewModel{Interrupted: true, Complete: false}, renderOpts{})
	count := 0
	for _, sec := range interrupted.Sections {
		if sec.Kind == sectionStatus {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("interrupted view has %d status sections, want exactly the interruption marker", count)
	}
	if !hasKind(interrupted, sectionStatus) {
		t.Fatal("interrupted view must carry an interruption status section")
	}

	complete := renderDoc(app.ViewModel{Complete: true}, renderOpts{})
	if !hasKind(complete, sectionStatus) {
		t.Fatal("complete view must have a status section")
	}

	truncated := renderDoc(app.ViewModel{Complete: true, Truncated: true}, renderOpts{})
	count = 0
	for _, sec := range truncated.Sections {
		if sec.Kind == sectionStatus {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("truncated complete view has %d status sections, want 2", count)
	}

	resuming := renderDoc(app.ViewModel{Resuming: true}, renderOpts{})
	if !hasKind(resuming, sectionStatus) {
		t.Fatal("resuming view must carry a status section")
	}
}

// Compaction notices render as their own section kind on the controller
// signal — never fabricated when the notice is absent.
func TestCompactionNoticeSection(t *testing.T) {
	if hasKind(renderDoc(app.ViewModel{}, renderOpts{}), sectionCompaction) {
		t.Fatal("no compaction notice must render no compaction section")
	}
	doc := renderDoc(app.ViewModel{Compaction: &app.CompactionNotice{
		Strategies: []string{"a", "b"}, DroppedGroups: 3, BeforeRunes: 9, AfterRunes: 4,
	}}, renderOpts{})
	if !hasKind(doc, sectionCompaction) {
		t.Fatal("compaction notice must render a compaction section")
	}
}

// flatten renders at most the first two lines of a collapsed section; the
// flags stay authoritative for tests, the flattening is the viewport text.
func TestFlattenCollapsedKeepsHeaderLines(t *testing.T) {
	doc := chatDoc{Sections: []chatSection{
		{Kind: sectionThinking, Collapsed: true, Lines: []string{"l1", "l2", "l3", "l4"}},
		{Kind: sectionText, Lines: []string{"t1", "t2"}},
	}}
	got := strings.Split(doc.flatten(), "\n")
	if len(got) != 4 { // 2 header lines + blank separator + 2 text lines
		t.Fatalf("flatten produced %d lines, want 4: %q", len(got), got)
	}
}
