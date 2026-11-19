package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/llm"
)

// benchCompactionHistory builds a history with toolGroups complete tool
// exchanges, each tool result carrying payloadRunes of text or one image of
// imageBytes raw bytes (base64-expanded by the JSON estimator), preceded by
// the system and first-user messages the compactor protects.
func benchCompactionHistory(b *testing.B, toolGroups, payloadRunes, imageBytes int) []llm.Message {
	b.Helper()
	history := []llm.Message{
		llm.SystemMessage("policy"),
		llm.UserMessage("first"),
	}
	for i := range toolGroups {
		toolUse := llm.ToolUseBlock{
			ID:    fmt.Sprintf("call-%d", i),
			Name:  "read",
			Input: []byte(`{"path":"main.go"}`),
		}
		result := llm.ToolResultBlock{
			Type:      "tool_result",
			ToolUseID: fmt.Sprintf("call-%d", i),
			Content:   strings.Repeat("result line\n", payloadRunes/12),
		}
		if imageBytes > 0 {
			result.Images = []llm.ImageBlock{{
				Type:     "image",
				MIMEType: "image/png",
				Data:     make([]byte, imageBytes),
			}}
		}
		history = append(history,
			llm.AssistantToolCallMessage(toolUse),
			llm.Message{Role: llm.RoleTool, Content: []llm.ContentBlock{result}},
		)
	}
	history = append(history,
		llm.AssistantMessage("old answer"),
		llm.UserMessage("latest"),
		llm.AssistantMessage("final"),
	)
	return history
}

// benchDropBudget returns a budget that forces dropping roughly half of the
// tool groups.
func benchDropBudget(b *testing.B, history []llm.Message, toolGroups, payloadRunes, imageBytes int) CompactionBudget {
	b.Helper()
	perGroup := estimateRunes(benchCompactionHistory(b, 1, payloadRunes, imageBytes)) - estimateRunes(benchCompactionHistory(b, 0, payloadRunes, imageBytes))
	total := estimateRunes(history)
	return CompactionBudget{MaxRunes: total - (toolGroups/2)*perGroup}
}

func benchmarkSlidingWindow(b *testing.B, toolGroups, payloadRunes, imageBytes int) {
	history := benchCompactionHistory(b, toolGroups, payloadRunes, imageBytes)
	budget := benchDropBudget(b, history, toolGroups, payloadRunes, imageBytes)
	b.ResetTimer()
	for b.Loop() {
		result, err := NewSlidingWindowCompactor().Compact(context.Background(), history, budget, nil, nil)
		if err != nil {
			b.Fatalf("Compact() unexpected error: %v", err)
		}
		if !result.Changed {
			b.Fatal("Compact() did not change history")
		}
	}
}

func BenchmarkSlidingWindowCompaction(b *testing.B) {
	b.Run("text/groups=50", func(b *testing.B) { benchmarkSlidingWindow(b, 50, 4096, 0) })
	b.Run("text/groups=100", func(b *testing.B) { benchmarkSlidingWindow(b, 100, 4096, 0) })
	b.Run("text/groups=200", func(b *testing.B) { benchmarkSlidingWindow(b, 200, 4096, 0) })
	b.Run("image/groups=25", func(b *testing.B) { benchmarkSlidingWindow(b, 25, 64, 512*1024) })
	b.Run("image/groups=50", func(b *testing.B) { benchmarkSlidingWindow(b, 50, 64, 512*1024) })
}

func benchmarkDropOldestToolGroups(b *testing.B, toolGroups, payloadRunes int) {
	history := benchCompactionHistory(b, toolGroups, payloadRunes, 0)
	budget := benchDropBudget(b, history, toolGroups, payloadRunes, 0)
	b.ResetTimer()
	for b.Loop() {
		result, err := NewDropOldestToolGroupsCompactor().Compact(context.Background(), history, budget, nil, nil)
		if err != nil {
			b.Fatalf("Compact() unexpected error: %v", err)
		}
		if !result.Changed {
			b.Fatal("Compact() did not change history")
		}
	}
}

// benchmarkUnevenSlidingWindow builds a history of small text groups with
// one large inline-image group in the middle, so the removal sequence
// crosses the expensive payload mid-pass.
func benchmarkUnevenSlidingWindow(b *testing.B, textGroups, imageBytes int) {
	history := []llm.Message{
		llm.SystemMessage("policy"),
		llm.UserMessage("first"),
	}
	var ids []string
	for i := range textGroups + 1 {
		ids = append(ids, fmt.Sprintf("call-%d", i))
	}
	for i, id := range ids {
		result := llm.ToolResultBlock{
			Type:      "tool_result",
			ToolUseID: id,
			Content:   strings.Repeat("result line\n", 340),
		}
		if i == textGroups/2 {
			result.Content = ""
			result.Images = []llm.ImageBlock{{
				Type:     "image",
				MIMEType: "image/png",
				Data:     make([]byte, imageBytes),
			}}
		}
		toolUse := llm.ToolUseBlock{ID: id, Name: "read", Input: []byte(`{"path":"main.go"}`)}
		history = append(history,
			llm.AssistantToolCallMessage(toolUse),
			llm.Message{Role: llm.RoleTool, Content: []llm.ContentBlock{result}},
		)
	}
	history = append(history,
		llm.AssistantMessage("old answer"),
		llm.UserMessage("latest"),
		llm.AssistantMessage("final"),
	)
	perGroup := estimateRunes(benchCompactionHistory(b, 1, 4080, 0)) - estimateRunes(benchCompactionHistory(b, 0, 4080, 0))
	total := estimateRunes(history)
	budget := CompactionBudget{MaxRunes: total - (textGroups+1)/2*perGroup}
	b.ResetTimer()
	for b.Loop() {
		result, err := NewSlidingWindowCompactor().Compact(context.Background(), history, budget, nil, nil)
		if err != nil {
			b.Fatalf("Compact() unexpected error: %v", err)
		}
		if !result.Changed {
			b.Fatal("Compact() did not change history")
		}
	}
}

func BenchmarkSlidingWindowUnevenPayload(b *testing.B) {
	b.Run("text+image/groups=25", func(b *testing.B) { benchmarkUnevenSlidingWindow(b, 24, 512*1024) })
}

func BenchmarkDropOldestToolGroupsCompaction(b *testing.B) {
	b.Run("text/groups=50", func(b *testing.B) { benchmarkDropOldestToolGroups(b, 50, 4096) })
	b.Run("text/groups=100", func(b *testing.B) { benchmarkDropOldestToolGroups(b, 100, 4096) })
}
