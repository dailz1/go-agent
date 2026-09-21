package tui

import "strings"

// sanitize removes terminal control sequences and unsafe control runes from
// model-derived text before it becomes display content. Model and tool
// output must never be rendered as a terminal control stream: CSI/OSC (and
// DCS/SOS/PM/APC) sequences are dropped whole, other C0 controls except
// newline and tab are dropped, and DEL is dropped. Tab is kept and expanded
// later by the renderer. Valid text — including CJK — passes through.
func sanitize(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	state := 0 // 0 normal, 1 after ESC, 2 CSI, 3 OSC/DCS-style, 4 ESC inside OSC
	for _, r := range s {
		switch state {
		case 0:
			switch {
			case r == 0x1b:
				state = 1
			case r == '\n' || r == '\t':
				b.WriteRune(r)
			case r < 0x20 || r == 0x7f:
				// CR and other control runes never reach the viewport.
			default:
				b.WriteRune(r)
			}
		case 1:
			switch r {
			case '[':
				state = 2
			case ']', 'P', 'X', '^', '_':
				state = 3
			default:
				// Two-character escape sequence: both halves dropped.
				state = 0
			}
		case 2:
			if r >= 0x40 && r <= 0x7e {
				state = 0 // final byte ends CSI
			}
			// Parameters and intermediates (0x20–0x3f) continue the sequence.
		case 3:
			if r == 0x07 {
				state = 0
				continue
			}
			if r == 0x1b {
				state = 4
			}
		case 4:
			// ESC \\ terminates string sequences; anything else keeps
			// consuming inside the sequence.
			if r == '\\' {
				state = 0
			} else {
				state = 3
			}
		}
	}
	return b.String()
}

// expandTabs converts tabs to four spaces so sanitized text has a single
// width interpretation inside the viewport.
func expandTabs(s string) string {
	if !strings.ContainsRune(s, '\t') {
		return s
	}
	return strings.ReplaceAll(s, "\t", "    ")
}
