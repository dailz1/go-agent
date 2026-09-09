package llm

// Truncate shortens s to at most maxLen runes, appending "..." if truncated.
func Truncate(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen]) + "..."
}
