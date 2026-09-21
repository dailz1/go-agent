package tui

import "strings"

// diffLine is one rendered diff row: ' ' context, '-' removed, '+' added.
type diffLine struct {
	Kind byte
	Text string
}

// maxDiffLines bounds a rendered diff; beyond it an explicit marker keeps
// the cut honest instead of silently hiding content.
const maxDiffLines = 2000

// lineDiff renders the changed region between two texts: common leading and
// trailing lines are trimmed, the middle shows removed versus added lines.
// It is deterministic and O(lines), which keeps approval previews cheap even
// for full-file writes; scrollable views render the result verbatim.
func lineDiff(before, after string) []diffLine {
	oldLines := strings.Split(before, "\n")
	newLines := strings.Split(after, "\n")

	lead := 0
	for lead < len(oldLines) && lead < len(newLines) && oldLines[lead] == newLines[lead] {
		lead++
	}
	tail := 0
	for tail < len(oldLines)-lead && tail < len(newLines)-lead &&
		oldLines[len(oldLines)-1-tail] == newLines[len(newLines)-1-tail] {
		tail++
	}
	oldMid := oldLines[lead : len(oldLines)-tail]
	newMid := newLines[lead : len(newLines)-tail]
	if len(oldMid) == 0 && len(newMid) == 0 {
		return nil
	}

	out := make([]diffLine, 0, len(oldMid)+len(newMid))
	for _, line := range oldMid {
		out = appendDiffLine(out, '-', line)
	}
	for _, line := range newMid {
		out = appendDiffLine(out, '+', line)
	}
	omitted := len(oldMid) + len(newMid) - maxDiffLines
	if omitted > 0 {
		marker := diffLine{Kind: '+', Text: strings.Replace(
			"(diff truncated: ~N more lines not shown)", "N", itoa(omitted), 1)}
		if len(out) == maxDiffLines {
			out = append(out[:maxDiffLines-1], marker)
		} else {
			out = append(out, marker)
		}
	}
	return out
}

func appendDiffLine(out []diffLine, kind byte, text string) []diffLine {
	if len(out) >= maxDiffLines {
		return out
	}
	return append(out, diffLine{Kind: kind, Text: text})
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
