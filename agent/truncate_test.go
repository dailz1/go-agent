package agent

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/dailz1/go-agent/tool"
)

func TestTruncateToolResult(t *testing.T) {
	invalidUTF8 := "\xff\xfe" + strings.Repeat("H", 98) + strings.Repeat("T", 100)
	invalidRunes := []rune(invalidUTF8)

	tests := []struct {
		name            string
		result          *tool.ToolResult
		maxRunes        int
		expectedContent string
		expectedOmitted int
		expectSame      bool
		expectValidUTF8 bool
		testIdempotence bool
	}{
		{
			name:            "empty string pass-through",
			result:          &tool.ToolResult{Content: ""},
			maxRunes:        128,
			expectedContent: "",
			expectedOmitted: 0,
			expectSame:      true,
			expectValidUTF8: true,
		},
		{
			name:            "short ASCII pass-through",
			result:          &tool.ToolResult{Content: "short tool result"},
			maxRunes:        128,
			expectedContent: "short tool result",
			expectedOmitted: 0,
			expectSame:      true,
			expectValidUTF8: true,
		},
		{
			name:            "exactly at limit pass-through",
			result:          &tool.ToolResult{Content: strings.Repeat("x", 128)},
			maxRunes:        128,
			expectedContent: strings.Repeat("x", 128),
			expectedOmitted: 0,
			expectSame:      true,
			expectValidUTF8: true,
		},
		{
			name:            "one over limit truncates",
			result:          &tool.ToolResult{Content: strings.Repeat("a", 129)},
			maxRunes:        128,
			expectedContent: strings.Repeat("a", 36) + "\n...[tool result truncated: omitted 57 of 129 runes]...\n" + strings.Repeat("a", 36),
			expectedOmitted: 57,
			expectValidUTF8: true,
		},
		{
			name:            "worked example",
			result:          &tool.ToolResult{Content: strings.Repeat("H", 100) + strings.Repeat("T", 100)},
			maxRunes:        100,
			expectedContent: strings.Repeat("H", 21) + "\n...[tool result truncated: omitted 157 of 200 runes]...\n" + strings.Repeat("T", 22),
			expectedOmitted: 157,
			expectValidUTF8: true,
		},
		{
			name:            "odd kept count gives extra rune to tail",
			result:          &tool.ToolResult{Content: strings.Repeat("A", 100) + strings.Repeat("B", 100)},
			maxRunes:        128,
			expectedContent: strings.Repeat("A", 35) + "\n...[tool result truncated: omitted 129 of 200 runes]...\n" + strings.Repeat("B", 36),
			expectedOmitted: 129,
			expectValidUTF8: true,
		},
		{
			name:            "CJK content uses rune boundaries",
			result:          &tool.ToolResult{Content: strings.Repeat("头", 100) + strings.Repeat("尾", 100)},
			maxRunes:        128,
			expectedContent: strings.Repeat("头", 35) + "\n...[tool result truncated: omitted 129 of 200 runes]...\n" + strings.Repeat("尾", 36),
			expectedOmitted: 129,
			expectValidUTF8: true,
		},
		{
			name:            "repeated emoji use rune boundaries",
			result:          &tool.ToolResult{Content: strings.Repeat("🚀", 100) + strings.Repeat("🎉", 100)},
			maxRunes:        128,
			expectedContent: strings.Repeat("🚀", 35) + "\n...[tool result truncated: omitted 129 of 200 runes]...\n" + strings.Repeat("🎉", 36),
			expectedOmitted: 129,
			expectValidUTF8: true,
		},
		{
			name:            "invalid UTF-8 becomes valid rune-counted output",
			result:          &tool.ToolResult{Content: invalidUTF8},
			maxRunes:        128,
			expectedContent: string(invalidRunes[:35]) + "\n...[tool result truncated: omitted 129 of 200 runes]...\n" + string(invalidRunes[164:]),
			expectedOmitted: 129,
			expectValidUTF8: true,
		},
		{
			name:            "truncation is idempotent",
			result:          &tool.ToolResult{Content: strings.Repeat("L", 100) + strings.Repeat("R", 100)},
			maxRunes:        128,
			expectedContent: strings.Repeat("L", 35) + "\n...[tool result truncated: omitted 129 of 200 runes]...\n" + strings.Repeat("R", 36),
			expectedOmitted: 129,
			expectValidUTF8: true,
			testIdempotence: true,
		},
		{
			name: "error status is preserved",
			result: &tool.ToolResult{
				Content: strings.Repeat("E", 200),
				Status:  tool.ResultError,
			},
			maxRunes:        128,
			expectedContent: strings.Repeat("E", 35) + "\n...[tool result truncated: omitted 129 of 200 runes]...\n" + strings.Repeat("E", 36),
			expectedOmitted: 129,
			expectValidUTF8: true,
		},
		{
			name: "data bytes are preserved",
			result: &tool.ToolResult{
				Content: strings.Repeat("D", 200),
				Data:    json.RawMessage(`{ "key": [1, 2, 3] }`),
			},
			maxRunes:        128,
			expectedContent: strings.Repeat("D", 35) + "\n...[tool result truncated: omitted 129 of 200 runes]...\n" + strings.Repeat("D", 36),
			expectedOmitted: 129,
			expectValidUTF8: true,
		},
		{
			name: "pass-through preserves pointer and fields",
			result: &tool.ToolResult{
				Content: "unchanged",
				Data:    json.RawMessage(`{"unchanged":true}`),
				Status:  tool.ResultError,
			},
			maxRunes:        128,
			expectedContent: "unchanged",
			expectedOmitted: 0,
			expectSame:      true,
			expectValidUTF8: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			originalContent := tt.result.Content
			originalData := append(json.RawMessage(nil), tt.result.Data...)
			originalStatus := tt.result.Status

			got, omitted := truncateToolResult(tt.result, tt.maxRunes)

			if omitted != tt.expectedOmitted {
				t.Errorf("omitted rune count = %d, want %d", omitted, tt.expectedOmitted)
			}
			if got.Content != tt.expectedContent {
				t.Errorf("content = %q, want %q", got.Content, tt.expectedContent)
			}
			if got.Status != originalStatus {
				t.Errorf("status = %q, want %q", got.Status, originalStatus)
			}
			if !bytes.Equal(got.Data, originalData) {
				t.Errorf("data = %q, want byte-identical %q", got.Data, originalData)
			}
			if tt.expectSame && got != tt.result {
				t.Error("pass-through returned a different pointer")
			}
			if !tt.expectSame && got == tt.result {
				t.Error("truncation returned the input pointer")
			}
			if tt.result.Content != originalContent {
				t.Errorf("input content mutated to %q, want %q", tt.result.Content, originalContent)
			}
			if tt.result.Status != originalStatus || !bytes.Equal(tt.result.Data, originalData) {
				t.Error("input status or data was mutated")
			}
			if utf8.RuneCountInString(got.Content) > tt.maxRunes {
				t.Errorf("output has %d runes, exceeds limit %d", utf8.RuneCountInString(got.Content), tt.maxRunes)
			}
			if tt.expectedOmitted > 0 && utf8.RuneCountInString(got.Content) != tt.maxRunes {
				t.Errorf("truncated output has %d runes, want exactly %d", utf8.RuneCountInString(got.Content), tt.maxRunes)
			}
			if tt.expectValidUTF8 && !utf8.ValidString(got.Content) {
				t.Error("output is not valid UTF-8")
			}

			if tt.testIdempotence {
				gotAgain, omittedAgain := truncateToolResult(got, tt.maxRunes)
				if gotAgain != got {
					t.Error("second truncation did not return the first result pointer")
				}
				if omittedAgain != 0 {
					t.Errorf("second truncation omitted %d runes, want 0", omittedAgain)
				}
				if gotAgain.Content != got.Content {
					t.Errorf("second truncation content = %q, want %q", gotAgain.Content, got.Content)
				}
			}
		})
	}
}
