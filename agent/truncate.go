package agent

import (
	"strconv"
	"unicode/utf8"

	"github.com/dailz1/go-agent/tool"
)

const minToolResultRunes = 128

// truncateToolResult shortens Content to at most maxRunes runes, keeping the
// head and tail halves around a marker that states the omitted count. It
// panics only on caller bugs: maxRunes must be at least minToolResultRunes
// so the marker always fits. The returned *ToolResult is a shallow copy when
// truncated; pass-through returns the original pointer.
func truncateToolResult(result *tool.ToolResult, maxRunes int) (*tool.ToolResult, int) {
	totalRunes := utf8.RuneCountInString(result.Content)
	if totalRunes <= maxRunes {
		return result, 0
	}

	totalDigits := len(strconv.Itoa(totalRunes))
	omittedDigits := 1
	var keptRunes int
	var omittedRunes int
	// The marker's rune count depends on the decimal width of O, so iterate
	// until the width stabilizes (digits only grow, hence convergence).
	for {
		markerRunes := 51 + omittedDigits + totalDigits
		keptRunes = maxRunes - markerRunes
		omittedRunes = totalRunes - keptRunes
		if keptRunes < 1 || omittedRunes < 0 {
			panic("agent: invalid tool result rune limit")
		}

		nextOmittedDigits := len(strconv.Itoa(omittedRunes))
		if nextOmittedDigits == omittedDigits {
			break
		}
		if nextOmittedDigits < omittedDigits {
			panic("agent: tool result truncation did not converge")
		}
		omittedDigits = nextOmittedDigits
	}

	marker := "\n...[tool result truncated: omitted " + strconv.Itoa(omittedRunes) +
		" of " + strconv.Itoa(totalRunes) + " runes]...\n"
	contentRunes := []rune(result.Content)
	headRunes := keptRunes / 2
	tailRunes := keptRunes - headRunes
	content := string(contentRunes[:headRunes]) + marker + string(contentRunes[totalRunes-tailRunes:])
	if utf8.RuneCountInString(content) != maxRunes {
		panic("agent: invalid truncated tool result length")
	}

	cpy := *result
	cpy.Content = content
	return &cpy, omittedRunes
}
