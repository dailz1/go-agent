package tui

import "testing"

func diffText(out []diffLine) (removed, added int) {
	for _, l := range out {
		switch l.Kind {
		case '-':
			removed++
		case '+':
			added++
		}
	}
	return removed, added
}

// The approval and restore panes show the changed region only: common
// leading and trailing lines are trimmed, the middle lists removed then
// added lines.
func TestLineDiffChangedRegionOnly(t *testing.T) {
	before := "a\nb\nc\nd\ne"
	after := "a\nb\nX\nd\ne"
	out := lineDiff(before, after)
	removed, added := diffText(out)
	if removed != 1 || added != 1 {
		t.Fatalf("removed=%d added=%d, want 1/1: %+v", removed, added, out)
	}
	if out[0].Text != "c" || out[1].Text != "X" {
		t.Fatalf("changed lines = %+v", out)
	}
}

func TestLineDiffInsertionAndDeletion(t *testing.T) {
	ins := lineDiff("a\nc", "a\nb\nc")
	if removed, added := diffText(ins); removed != 0 || added != 1 {
		t.Fatalf("insertion: removed=%d added=%d, want 0/1", removed, added)
	}
	del := lineDiff("a\nb\nc", "a\nc")
	if removed, added := diffText(del); removed != 1 || added != 0 {
		t.Fatalf("deletion: removed=%d added=%d, want 1/0", removed, added)
	}
}

func TestLineDiffIdentical(t *testing.T) {
	if out := lineDiff("same\nsame", "same\nsame"); out != nil {
		t.Fatalf("identical texts must produce no diff, got %+v", out)
	}
}

// An empty side is still one line (the empty line), so a new file shows
// the empty before-line as removed and a full delete shows the empty
// after-line as added.
func TestLineDiffEmptySides(t *testing.T) {
	out := lineDiff("", "new\nfile")
	if removed, added := diffText(out); removed != 1 || added != 2 {
		t.Fatalf("new file: removed=%d added=%d, want 1/2", removed, added)
	}
	back := lineDiff("old\nfile", "")
	if removed, added := diffText(back); removed != 2 || added != 1 {
		t.Fatalf("delete all: removed=%d added=%d, want 2/1", removed, added)
	}
}

// Diffs beyond the line cap are cut and the cut is marked by replacing the
// last kept line — the length bound is the asserted contract.
func TestLineDiffCap(t *testing.T) {
	n := maxDiffLines * 2
	before := make([]string, n)
	after := make([]string, n)
	for i := range before {
		before[i] = "x"
		after[i] = "y"
	}
	out := lineDiff(joinLines(before), joinLines(after))
	if len(out) != maxDiffLines {
		t.Fatalf("capped diff has %d lines, want exactly %d", len(out), maxDiffLines)
	}
	if out[len(out)-1].Kind != '+' {
		t.Fatalf("last capped line kind = %q, want '+' marker", out[len(out)-1].Kind)
	}
}

func joinLines(lines []string) string {
	out := ""
	for i, l := range lines {
		if i > 0 {
			out += "\n"
		}
		out += l
	}
	return out
}
