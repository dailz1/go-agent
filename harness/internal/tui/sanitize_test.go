package tui

import "testing"

// Model- and tool-derived text must never render as a terminal control
// stream: CSI/OSC (and DCS/SOS/PM/APC) sequences drop whole, other control
// runes drop, valid text including CJK passes through unchanged.
func TestSanitizeStripsControlSequences(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain ascii", "hello world", "hello world"},
		{"cjk passes", "你好，世界", "你好，世界"},
		{"csi color", "a\x1b[31mb", "ab"},
		{"csi multi-param", "\x1b[1;32;40mx\x1b[0m", "x"},
		{"osc bel terminated", "\x1b]0;title\x07after", "after"},
		{"osc st terminated", "\x1b]8;;http://x\x1b\\text", "text"},
		{"dcs st terminated", "a\x1bP1;2q\x1b\\b", "ab"},
		{"unterminated dcs swallows rest", "a\x1bP1;2q datab", "a"},
		{"two-char escape", "a\x1bMb", "ab"},
		{"bare escape at end", "abc\x1b", "abc"},
		{"cr dropped", "a\rb", "ab"},
		{"other c0 dropped", "a\x00\x01\x02b", "ab"},
		{"del dropped", "a\x7fb", "ab"},
		{"newline and tab kept", "a\nb\tc", "a\nb\tc"},
		{"csi keeps consuming intermediates", "\x1b[?25lh", "h"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitize(tt.in); got != tt.want {
				t.Fatalf("sanitize(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestExpandTabs(t *testing.T) {
	if got := expandTabs("no tabs"); got != "no tabs" {
		t.Fatalf("expandTabs changed a tab-free string: %q", got)
	}
	if got, want := expandTabs("a\tb"), "a    b"; got != want {
		t.Fatalf("expandTabs = %q, want %q", got, want)
	}
}
