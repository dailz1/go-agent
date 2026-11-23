package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

func TestCompactorPartitionHistory(t *testing.T) {
	t.Parallel()

	systemOne := llm.SystemMessage("first system")
	systemTwo := llm.SystemMessage("second system")
	parallelAssistant, parallelResults := compactorToolExchange(
		compactorToolCall{id: "call-1", name: "read", result: "one"},
		compactorToolCall{id: "call-2", name: "write", result: "two"},
	)
	hybridAssistant := llm.Message{
		Role: llm.RoleAssistant,
		Content: []llm.ContentBlock{
			&llm.TextBlock{Type: "text", Text: "I will inspect both files."},
			&llm.ToolUseBlock{Type: "tool_use", ID: "hybrid", Name: "read", Input: json.RawMessage(`{"path":"a"}`)},
		},
	}
	hybridResult := compactorToolResult("hybrid", "contents")

	tests := []struct {
		name            string
		history         []llm.Message
		expectedSizes   []int
		expectedSystems int
		expectedTools   []bool
		expectedErr     error
	}{
		{
			name:          "empty history",
			history:       []llm.Message{},
			expectedSizes: []int{},
		},
		{
			name:          "nil history",
			history:       nil,
			expectedSizes: []int{},
		},
		{
			name:          "no system prefix",
			history:       []llm.Message{llm.UserMessage("question"), llm.AssistantMessage("answer")},
			expectedSizes: []int{1, 1},
			expectedTools: []bool{false, false},
		},
		{
			name:            "single leading system",
			history:         []llm.Message{systemOne, llm.UserMessage("question")},
			expectedSizes:   []int{1, 1},
			expectedSystems: 1,
			expectedTools:   []bool{false, false},
		},
		{
			name:            "multiple leading systems",
			history:         []llm.Message{systemOne, systemTwo, llm.UserMessage("question")},
			expectedSizes:   []int{2, 1},
			expectedSystems: 2,
			expectedTools:   []bool{false, false},
		},
		{
			name:        "system after prefix",
			history:     []llm.Message{systemOne, llm.UserMessage("question"), systemTwo},
			expectedErr: ErrInvalidHistory,
		},
		{
			name:        "orphan tool result",
			history:     []llm.Message{llm.UserMessage("question"), compactorToolResult("orphan", "result")},
			expectedErr: ErrInvalidHistory,
		},
		{
			name:          "parallel tool calls form one group",
			history:       append([]llm.Message{llm.UserMessage("question"), parallelAssistant}, parallelResults...),
			expectedSizes: []int{1, 3},
			expectedTools: []bool{false, true},
		},
		{
			name:          "hybrid assistant and result are atomic",
			history:       []llm.Message{llm.UserMessage("question"), hybridAssistant, hybridResult},
			expectedSizes: []int{1, 2},
			expectedTools: []bool{false, true},
		},
		{
			name: "missing result id",
			history: []llm.Message{
				llm.UserMessage("question"), parallelAssistant, parallelResults[0],
			},
			expectedErr: ErrInvalidHistory,
		},
		{
			name: "unknown result id",
			history: []llm.Message{
				llm.UserMessage("question"), parallelAssistant, parallelResults[0], compactorToolResult("other", "result"),
			},
			expectedErr: ErrInvalidHistory,
		},
		{
			name: "duplicate result id",
			history: []llm.Message{
				llm.UserMessage("question"), parallelAssistant, parallelResults[0], parallelResults[0], parallelResults[1],
			},
			expectedErr: ErrInvalidHistory,
		},
		{
			name: "empty tool use id",
			history: []llm.Message{
				llm.UserMessage("question"),
				llm.AssistantToolCallMessage(&llm.ToolUseBlock{ID: "", Name: "read", Input: json.RawMessage(`{}`)}),
			},
			expectedErr: ErrInvalidHistory,
		},
		{
			name: "duplicate tool use id",
			history: []llm.Message{
				llm.UserMessage("question"),
				llm.AssistantToolCallMessage(
					&llm.ToolUseBlock{ID: "same", Name: "read", Input: json.RawMessage(`{}`)},
					&llm.ToolUseBlock{ID: "same", Name: "write", Input: json.RawMessage(`{}`)},
				),
				compactorToolResult("same", "result"),
			},
			expectedErr: ErrInvalidHistory,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			groups, err := partitionHistory(tt.history)
			if tt.expectedErr != nil {
				if !errors.Is(err, tt.expectedErr) {
					t.Fatalf("partitionHistory() error = %v, want errors.Is(_, %v)", err, tt.expectedErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("partitionHistory() unexpected error: %v", err)
			}
			if len(groups) != len(tt.expectedSizes) {
				t.Fatalf("partitionHistory() returned %d groups, want %d", len(groups), len(tt.expectedSizes))
			}
			for i, expectedSize := range tt.expectedSizes {
				if len(groups[i].messages) != expectedSize {
					t.Errorf("group %d has %d messages, want %d", i, len(groups[i].messages), expectedSize)
				}
				if groups[i].isTool != tt.expectedTools[i] {
					t.Errorf("group %d isTool = %t, want %t", i, groups[i].isTool, tt.expectedTools[i])
				}
			}
			if tt.expectedSystems > 0 {
				if !reflect.DeepEqual(groups[0].messages, tt.history[:tt.expectedSystems]) {
					t.Errorf("system prefix changed: got %#v, want %#v", groups[0].messages, tt.history[:tt.expectedSystems])
				}
			}
		})
	}
}

func TestCompactorEstimateRunes(t *testing.T) {
	t.Parallel()

	validHistory := []llm.Message{
		llm.SystemMessage("ASCII"),
		llm.UserMessage("中文"),
		llm.AssistantMessage("emoji 😀"),
		llm.AssistantToolCallMessage(&llm.ToolUseBlock{
			ID:    "raw",
			Name:  "inspect",
			Input: json.RawMessage(`{"ключ":"значение"}`),
		}),
		compactorToolResult("raw", "done"),
	}
	encoded, err := json.Marshal(validHistory)
	if err != nil {
		t.Fatalf("json.Marshal(validHistory): %v", err)
	}
	expectedValid := utf8.RuneCount(encoded)

	invalidInput := json.RawMessage(`{"broken":`)
	invalidHistory := []llm.Message{
		{
			Role: llm.RoleAssistant,
			Content: []llm.ContentBlock{
				&llm.TextBlock{Type: "text", Text: "中文😀"},
				&llm.ToolUseBlock{Type: "tool_use", ID: "bad", Name: "lookup", Input: invalidInput},
				&llm.ToolResultBlock{Type: "tool_result", ToolUseID: "bad", Content: "result"},
				&llm.ReasoningBlock{Type: "reasoning", Content: "reason"},
				&llm.ImageBlock{Type: "image", Data: []byte{1, 2, 3}},
			},
		},
	}
	expectedFallback := len(llm.RoleAssistant) + len("中文😀") + len("lookup") + len(invalidInput) + len("result") + len("reason") + 3

	tests := []struct {
		name     string
		history  []llm.Message
		expected int
	}{
		{name: "valid json counts encoded runes", history: validHistory, expected: expectedValid},
		{name: "invalid raw message uses manual fallback", history: invalidHistory, expected: expectedFallback},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			first := estimateRunes(tt.history)
			second := estimateRunes(tt.history)
			if first != tt.expected {
				t.Errorf("estimateRunes() = %d, want %d", first, tt.expected)
			}
			if second != first {
				t.Errorf("estimateRunes() second call = %d, want deterministic result %d", second, first)
			}
		})
	}
}

func TestCompactorThresholdSemantics(t *testing.T) {
	t.Parallel()

	history := []llm.Message{llm.UserMessage("question"), llm.AssistantMessage(strings.Repeat("answer", 20))}

	tests := []struct {
		name          string
		budget        int
		expectChanged bool
	}{
		{name: "equal budget is unchanged", budget: estimateRunes(history), expectChanged: false},
		{name: "larger budget is unchanged", budget: estimateRunes(history) + 1, expectChanged: false},
		{name: "over budget runs strategy", budget: estimateRunes(history) - 1, expectChanged: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			calls := 0
			strategy := CompactFunc(func(_ context.Context, input []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
				calls++
				return CompactionResult{
					History:    []llm.Message{input[0]},
					Strategies: []string{"recording"},
					Changed:    true,
				}, nil
			})
			result, err := NewCompactorChain(strategy).Compact(
				context.Background(),
				history,
				CompactionBudget{MaxRunes: tt.budget},
				nil,
				nil,
			)
			if err != nil {
				t.Fatalf("Compact() unexpected error: %v", err)
			}
			if result.Changed != tt.expectChanged {
				t.Errorf("Compact() Changed = %t, want %t", result.Changed, tt.expectChanged)
			}
			if tt.expectChanged && calls != 1 {
				t.Errorf("strategy calls = %d, want 1", calls)
			}
			if !tt.expectChanged {
				if calls != 0 {
					t.Errorf("strategy calls = %d, want 0", calls)
				}
				if !reflect.DeepEqual(result.History, history) {
					t.Errorf("unchanged history = %#v, want %#v", result.History, history)
				}
			}
		})
	}
}

func TestCompactorDropOldestToolGroups(t *testing.T) {
	t.Parallel()

	oldAssistant, oldResults := compactorToolExchange(compactorToolCall{id: "old", name: "lookup", result: "old first line\nold detail"})
	newAssistant, newResults := compactorToolExchange(compactorToolCall{id: "new", name: "fetch", result: "new first line\nnew detail"})
	base := []llm.Message{
		llm.SystemMessage("policy"),
		llm.UserMessage("first user"),
		oldAssistant,
		oldResults[0],
		llm.AssistantMessage("middle"),
		newAssistant,
		newResults[0],
		llm.UserMessage("latest user"),
		llm.AssistantMessage("final protected"),
	}

	t.Run("oldest eligible group is collapsed first", func(t *testing.T) {
		t.Parallel()

		firstCollapsed := []llm.Message{
			base[0], base[1], llm.AssistantMessage("[Tool Calls]\nlookup: old first line"),
			base[4], base[5], base[6], base[7], base[8],
		}
		budget := estimateRunes(firstCollapsed)
		result, err := NewDropOldestToolGroupsCompactor().Compact(
			context.Background(),
			base,
			CompactionBudget{MaxRunes: budget},
			nil,
			nil,
		)
		if err != nil {
			t.Fatalf("Compact() unexpected error: %v", err)
		}
		if !reflect.DeepEqual(result.History, firstCollapsed) {
			t.Errorf("Compact() history = %#v, want oldest-only collapse %#v", result.History, firstCollapsed)
		}
		if result.DroppedGroups != 1 || !result.Changed {
			t.Errorf("Compact() DroppedGroups, Changed = %d, %t; want 1, true", result.DroppedGroups, result.Changed)
		}
		if !reflect.DeepEqual(result.Strategies, []string{"drop_oldest_tool_groups"}) {
			t.Errorf("Compact() Strategies = %#v", result.Strategies)
		}
	})

	t.Run("protected groups are never touched", func(t *testing.T) {
		t.Parallel()

		protectedAssistant, protectedResults := compactorToolExchange(compactorToolCall{
			id: "final", name: "final-tool", result: strings.Repeat("protected", 100),
		})
		history := []llm.Message{
			llm.SystemMessage("policy"),
			llm.UserMessage("first"),
			oldAssistant, oldResults[0],
			llm.UserMessage("latest"),
			protectedAssistant, protectedResults[0],
		}
		result, _ := NewDropOldestToolGroupsCompactor().Compact(
			context.Background(),
			history,
			CompactionBudget{MaxRunes: 0},
			nil,
			nil,
		)
		if len(result.History) != 6 {
			t.Fatalf("Compact() returned %d messages, want 6", len(result.History))
		}
		if !reflect.DeepEqual(result.History[len(result.History)-2:], history[len(history)-2:]) {
			t.Errorf("final protected tool group changed")
		}
		if !reflect.DeepEqual(result.History[:2], history[:2]) {
			t.Errorf("system or first user changed")
		}
	})

	t.Run("replacement must be smaller", func(t *testing.T) {
		t.Parallel()

		assistant := llm.AssistantToolCallMessage(&llm.ToolUseBlock{
			ID: "x", Name: "n", Input: json.RawMessage(`{`),
		})
		resultMessage := compactorToolResult("x", "r")
		history := []llm.Message{
			llm.UserMessage("first"), assistant, resultMessage, llm.UserMessage("latest"), llm.AssistantMessage("final"),
		}
		result, err := NewDropOldestToolGroupsCompactor().Compact(
			context.Background(),
			history,
			CompactionBudget{MaxRunes: 0},
			nil,
			nil,
		)
		if !errors.Is(err, ErrCompactionBudgetExceeded) {
			t.Fatalf("Compact() error = %v, want ErrCompactionBudgetExceeded", err)
		}
		if result.Changed || !reflect.DeepEqual(result.History, history) {
			t.Errorf("non-smaller replacement changed history: %#v", result.History)
		}
	})

	t.Run("replacement is capped at 4096 runes", func(t *testing.T) {
		t.Parallel()

		calls := make([]compactorToolCall, 0, 80)
		for i := range 80 {
			calls = append(calls, compactorToolCall{
				id: string(rune('a' + i)), name: strings.Repeat("tool", 20), result: strings.Repeat("result", 20),
			})
		}
		assistant, results := compactorToolExchange(calls...)
		history := append([]llm.Message{llm.UserMessage("first"), assistant}, results...)
		history = append(history, llm.UserMessage("latest"), llm.AssistantMessage("final"))
		result, _ := NewDropOldestToolGroupsCompactor().Compact(
			context.Background(),
			history,
			CompactionBudget{MaxRunes: 0},
			nil,
			nil,
		)
		text := compactorText(result.History[1])
		if utf8.RuneCountInString(text) > 4096 {
			t.Errorf("collapsed marker has %d runes, want <= 4096", utf8.RuneCountInString(text))
		}
		if !strings.Contains(text, "... [truncated]") {
			t.Errorf("collapsed marker %q does not contain truncation marker", text)
		}
	})

	t.Run("second compaction is idempotent", func(t *testing.T) {
		t.Parallel()

		budget := estimateRunes(base) - 1
		first, _ := NewDropOldestToolGroupsCompactor().Compact(
			context.Background(),
			base,
			CompactionBudget{MaxRunes: budget},
			nil,
			nil,
		)
		budget = estimateRunes(first.History)
		second, err := NewDropOldestToolGroupsCompactor().Compact(
			context.Background(),
			first.History,
			CompactionBudget{MaxRunes: budget},
			nil,
			nil,
		)
		if err != nil {
			t.Fatalf("second Compact() unexpected error: %v", err)
		}
		if second.Changed || !reflect.DeepEqual(second.History, first.History) {
			t.Errorf("second Compact() = %#v, want unchanged", second)
		}
	})
}

func TestGroupSizeAccountingMatchesWholeEncoding(t *testing.T) {
	t.Parallel()

	assistant, results := compactorToolExchange(
		compactorToolCall{id: "a", name: "read", result: strings.Repeat("content", 40)},
		compactorToolCall{id: "b", name: "read", result: strings.Repeat("content", 60)},
	)
	history := []llm.Message{
		llm.SystemMessage("policy"),
		llm.UserMessage("first"),
		assistant,
		results[0],
		results[1],
		llm.AssistantMessage("old"),
		llm.UserMessage("latest"),
	}
	groups, err := partitionHistory(history)
	if err != nil {
		t.Fatalf("partitionHistory() unexpected error: %v", err)
	}
	sizes := make([]int, len(groups))
	for index := range groups {
		sizes[index] = groupEstimate(groups[index].messages, true)
	}

	entry := estimateRunes(history)
	active := make([]bool, len(groups))
	for index := range active {
		active[index] = true
	}
	droppedDelta := 0
	for _, dropped := range []int{2, 3} {
		active[dropped] = false
		droppedDelta += dropDelta(sizes[dropped], true)
	}
	remaining := flattenActiveGroups(groups, active)
	if got, want := entry-droppedDelta, estimateRunes(remaining); got != want {
		t.Errorf("entry %d - dropped delta %d = %d, want re-encoded remainder %d", entry, droppedDelta, entry-droppedDelta, want)
	}
	if got, want := dropDelta(sizes[0], false), sizes[0]; got != want {
		t.Errorf("dropDelta(fallback) = %d, want %d", got, want)
	}
}
func TestCompactorSlidingWindowDropsExactlyToBudget(t *testing.T) {
	t.Parallel()

	ids := []string{"a", "b", "c", "d", "e", "f"}
	history := []llm.Message{llm.SystemMessage("policy"), llm.UserMessage("first")}
	for _, id := range ids {
		assistant, results := compactorToolExchange(compactorToolCall{id: id, name: "read", result: strings.Repeat("payload", 20)})
		history = append(history, assistant, results[0])
	}
	history = append(history, llm.AssistantMessage("old"), llm.UserMessage("latest"), llm.AssistantMessage("final"))

	groups, err := partitionHistory(history)
	if err != nil {
		t.Fatalf("partitionHistory() unexpected error: %v", err)
	}
	protected := protectedGroups(groups)
	entry := estimateRunes(history)
	eligibleSizes := []int{}
	for index, group := range groups {
		if !protected[index] {
			eligibleSizes = append(eligibleSizes, groupEstimate(group.messages, true))
		}
	}
	if len(eligibleSizes) < 5 {
		t.Fatalf("partitionHistory() eligible groups = %d, want at least 5", len(eligibleSizes))
	}
	afterThree := entry - (dropDelta(eligibleSizes[0], true) + dropDelta(eligibleSizes[1], true) + dropDelta(eligibleSizes[2], true))

	tests := []struct {
		name    string
		budget  int
		dropped int
	}{
		{name: "drops exactly three", budget: afterThree, dropped: 3},
		{name: "one rune over forces a fourth drop", budget: afterThree - 1, dropped: 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result, err := NewSlidingWindowCompactor().Compact(
				context.Background(),
				history,
				CompactionBudget{MaxRunes: tt.budget},
				nil,
				nil,
			)
			if err != nil {
				t.Fatalf("Compact() unexpected error: %v", err)
			}
			if result.DroppedGroups != tt.dropped {
				t.Errorf("Compact() DroppedGroups = %d, want %d", result.DroppedGroups, tt.dropped)
			}
			if got := estimateRunes(result.History); got > tt.budget {
				t.Errorf("Compact() history estimate = %d, over budget %d", got, tt.budget)
			}
		})
	}
}

type compactCustomMarshalBlock struct {
	*llm.TextBlock
}

func (c compactCustomMarshalBlock) MarshalJSON() ([]byte, error) {
	return []byte(`{"type":"text","text":"` + c.Text + `"}`), nil
}

func TestKnownBlockTypesGate(t *testing.T) {
	t.Parallel()

	if !knownBlockTypes([]llm.Message{llm.UserMessage("plain")}) {
		t.Error("knownBlockTypes() = false for known types, want true")
	}
	history := []llm.Message{llm.UserMessage("mixed")}
	history[0].Content = append(history[0].Content, compactCustomMarshalBlock{TextBlock: &llm.TextBlock{Text: "custom"}})
	if knownBlockTypes(history) {
		t.Error("knownBlockTypes() = true for embedding-wrapper block, want false")
	}
}

func TestCompactorSlidingWindowCustomMarshalBlock(t *testing.T) {
	t.Parallel()

	assistant, results := compactorToolExchange(compactorToolCall{id: "tool", name: "read", result: strings.Repeat("result", 30)})
	history := []llm.Message{
		llm.SystemMessage("policy"),
		llm.UserMessage("first"),
		assistant,
		results[0],
		llm.AssistantMessage("old"),
		llm.UserMessage("latest"),
	}
	history[4].Content = append(history[4].Content, compactCustomMarshalBlock{TextBlock: &llm.TextBlock{Text: "custom"}})

	result, err := NewSlidingWindowCompactor().Compact(
		context.Background(),
		history,
		CompactionBudget{MaxRunes: estimateRunes(history) - 1},
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("Compact() unexpected error: %v", err)
	}
	if result.DroppedGroups != 1 {
		t.Errorf("Compact() DroppedGroups = %d, want 1", result.DroppedGroups)
	}
	if got := estimateRunes(result.History); got > estimateRunes(history)-1 {
		t.Errorf("Compact() history estimate = %d, over budget", got)
	}
}

func TestJSONSizeLowerBoundBelowActualEncoding(t *testing.T) {
	t.Parallel()

	blocks := []llm.ContentBlock{
		&llm.TextBlock{Text: strings.Repeat("中文内容", 100)},
		&llm.TextBlock{Text: "{  padded  }"},
		&llm.ToolUseBlock{ID: "a", Name: "read", Input: json.RawMessage(`{  "x" : 1  }`)},
		&llm.ToolResultBlock{Type: "tool_result", ToolUseID: "a", Content: strings.Repeat("结果", 50), Images: []llm.ImageBlock{{Type: "image", Data: make([]byte, 1000)}}},
		&llm.ToolResultBlock{Type: "tool_result", ToolUseID: "a", Content: "result"},
		&llm.ReasoningBlock{Type: "reasoning", Content: strings.Repeat("思考", 30)},
		&llm.ReasoningBlock{Type: "reasoning", Content: "thinking"},
		&llm.ReasoningItemBlock{Type: "reasoning_item", EncryptedContent: strings.Repeat("密", 100), Summary: []string{"摘要"}},
		&llm.ImageBlock{Type: "image", Data: make([]byte, 500)},
		llm.AssistantToolCallMessage(&llm.ToolUseBlock{ID: "b", Name: "w", Input: json.RawMessage(`{}`)}).Content[0],
	}
	group := []llm.Message{{Role: llm.RoleAssistant, Content: blocks}}
	bound := jsonSizeLowerBound(group)
	actual := estimateRunes(group)
	if bound > actual {
		t.Errorf("jsonSizeLowerBound() = %d exceeds actual encoding %d", bound, actual)
	}
	if bound <= 0 {
		t.Errorf("jsonSizeLowerBound() = %d, want positive", bound)
	}
}

func TestCompactorSlidingWindow(t *testing.T) {
	t.Parallel()

	assistant, results := compactorToolExchange(compactorToolCall{id: "tool", name: "read", result: strings.Repeat("result", 30)})
	history := []llm.Message{
		llm.SystemMessage("policy"),
		llm.UserMessage("first"),
		assistant,
		results[0],
		llm.AssistantMessage(strings.Repeat("old", 40)),
		llm.UserMessage("latest"),
		llm.AssistantMessage("final"),
	}
	expectedAfterOne := []llm.Message{history[0], history[1], history[4], history[5], history[6]}

	tests := []struct {
		name     string
		budget   int
		expected []llm.Message
		dropped  int
	}{
		{
			name:     "removes a complete oldest tool group",
			budget:   estimateRunes(expectedAfterOne),
			expected: expectedAfterOne,
			dropped:  1,
		},
		{
			name:     "protects system first user latest user and final group",
			budget:   0,
			expected: []llm.Message{history[0], history[1], history[5], history[6]},
			dropped:  2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result, err := NewSlidingWindowCompactor().Compact(
				context.Background(),
				history,
				CompactionBudget{MaxRunes: tt.budget},
				nil,
				nil,
			)
			if tt.budget == 0 {
				if !errors.Is(err, ErrCompactionBudgetExceeded) {
					t.Fatalf("Compact() error = %v, want ErrCompactionBudgetExceeded", err)
				}
			} else if err != nil {
				t.Fatalf("Compact() unexpected error: %v", err)
			}
			if !reflect.DeepEqual(result.History, tt.expected) {
				t.Errorf("Compact() history = %#v, want %#v", result.History, tt.expected)
			}
			if result.DroppedGroups != tt.dropped {
				t.Errorf("Compact() DroppedGroups = %d, want %d", result.DroppedGroups, tt.dropped)
			}
			if !reflect.DeepEqual(result.Strategies, []string{"sliding_window"}) {
				t.Errorf("Compact() Strategies = %#v", result.Strategies)
			}
		})
	}
}

func TestCompactorSummarization(t *testing.T) {
	t.Parallel()

	assistant, results := compactorToolExchange(compactorToolCall{id: "call", name: "read", result: "file contents"})
	history := []llm.Message{
		llm.SystemMessage("policy one"),
		llm.SystemMessage("policy two"),
		llm.UserMessage("first"),
		llm.AssistantMessage("old answer"),
		assistant,
		results[0],
		llm.UserMessage("intermediate user"),
		llm.AssistantMessage(strings.Repeat("more history ", 60)),
		llm.UserMessage("latest"),
		llm.AssistantMessage("current round"),
	}

	t.Run("success replaces entire span and preserves request systems", func(t *testing.T) {
		t.Parallel()

		provider := &compactorProvider{response: &llm.Message{
			Role: llm.RoleTool,
			Content: []llm.ContentBlock{
				&llm.TextBlock{Type: "text", Text: "summary one"},
				&llm.ReasoningBlock{Type: "reasoning", Content: "ignored"},
				&llm.TextBlock{Type: "text", Text: "summary two"},
			},
		}}
		result, err := NewSummarizationCompactor().Compact(
			context.Background(),
			history,
			CompactionBudget{MaxRunes: 1300},
			provider,
			nil,
		)
		if err != nil && !errors.Is(err, ErrCompactionBudgetExceeded) {
			t.Fatalf("Compact() unexpected error: %v", err)
		}
		expected := []llm.Message{
			history[0], history[1], history[2],
			llm.AssistantMessage("[Summary]\nsummary one\nsummary two"),
			history[8], history[9],
		}
		if !reflect.DeepEqual(result.History, expected) {
			t.Errorf("Compact() history = %#v, want %#v", result.History, expected)
		}
		if len(provider.requests) != 1 {
			t.Fatalf("provider calls = %d, want 1", len(provider.requests))
		}
		request := provider.requests[0]
		if !reflect.DeepEqual(request[:2], history[:2]) {
			t.Errorf("provider system prefix = %#v, want %#v", request[:2], history[:2])
		}
		prompt := compactorText(request[2])
		for _, required := range []string{
			"Summarize the following conversation:",
			defaultSummarizationPrompt,
			"assistant: old answer",
			"assistant: read:",
			"tool: file contents",
			"user: intermediate user",
			"assistant: more history",
		} {
			if !strings.Contains(prompt, required) {
				t.Errorf("summarization prompt does not contain %q: %q", required, prompt)
			}
		}
	})

	tests := []struct {
		name     string
		provider *compactorProvider
	}{
		{name: "nil response", provider: &compactorProvider{}},
		{
			name: "response without text",
			provider: &compactorProvider{response: &llm.Message{
				Role: llm.RoleAssistant,
				Content: []llm.ContentBlock{
					&llm.ReasoningBlock{Type: "reasoning", Content: "not summary text"},
				},
			}},
		},
		{name: "provider error", provider: &compactorProvider{err: errors.New("provider unavailable")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result, err := NewSummarizationCompactor().Compact(
				context.Background(),
				history,
				CompactionBudget{MaxRunes: 0},
				tt.provider,
				nil,
			)
			if err == nil {
				t.Fatal("Compact() error = nil, want error")
			}
			if result.Changed || !reflect.DeepEqual(result.History, history) {
				t.Errorf("failed summarization changed history: %#v", result.History)
			}
		})
	}

	t.Run("empty span is a no-op", func(t *testing.T) {
		t.Parallel()

		provider := &compactorProvider{response: compactorMessagePointer(llm.AssistantMessage("unused"))}
		shortHistory := []llm.Message{llm.SystemMessage("policy"), llm.UserMessage("only"), llm.AssistantMessage("final")}
		result, err := NewSummarizationCompactor().Compact(
			context.Background(),
			shortHistory,
			CompactionBudget{MaxRunes: 0},
			provider,
			nil,
		)
		if err != nil {
			t.Fatalf("Compact() unexpected error: %v", err)
		}
		if result.Changed || !reflect.DeepEqual(result.History, shortHistory) {
			t.Errorf("empty-span Compact() = %#v, want unchanged", result)
		}
		if len(provider.requests) != 0 {
			t.Errorf("provider calls = %d, want 0", len(provider.requests))
		}
	})
}

func TestCompactorChain(t *testing.T) {
	t.Parallel()

	history := []llm.Message{llm.UserMessage("first"), llm.AssistantMessage(strings.Repeat("history", 200)), llm.UserMessage("latest"), llm.AssistantMessage("final")}

	t.Run("runs in order and stops when caller budget is reached", func(t *testing.T) {
		t.Parallel()

		calls := []string{}
		first := CompactFunc(func(_ context.Context, input []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
			calls = append(calls, "drop")
			return CompactionResult{History: append([]llm.Message{}, input...), Strategies: []string{"drop"}}, nil
		})
		secondHistory := []llm.Message{history[0], history[2], history[3]}
		second := CompactFunc(func(_ context.Context, _ []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
			calls = append(calls, "window")
			return CompactionResult{History: secondHistory, Strategies: []string{"window"}, DroppedGroups: 1, Changed: true}, nil
		})
		third := CompactFunc(func(_ context.Context, input []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
			calls = append(calls, "third")
			return CompactionResult{History: input}, nil
		})
		result, err := NewCompactorChain(first, second, third).Compact(
			context.Background(),
			history,
			CompactionBudget{MaxRunes: estimateRunes(secondHistory)},
			nil,
			nil,
		)
		if err != nil {
			t.Fatalf("Compact() unexpected error: %v", err)
		}
		if !reflect.DeepEqual(calls, []string{"drop", "window"}) {
			t.Errorf("strategy calls = %#v, want drop then window only", calls)
		}
		if !reflect.DeepEqual(result.History, secondHistory) {
			t.Errorf("Compact() history = %#v, want %#v", result.History, secondHistory)
		}
		if !reflect.DeepEqual(result.Strategies, []string{"drop", "window"}) {
			t.Errorf("Compact() Strategies = %#v", result.Strategies)
		}
	})

	t.Run("strategy error degrades to next and is recorded", func(t *testing.T) {
		t.Parallel()

		strategyErr := errors.New("summarizer offline")
		failed := CompactFunc(func(_ context.Context, input []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
			return CompactionResult{History: input, Strategies: []string{"failed"}}, strategyErr
		})
		recoveredHistory := []llm.Message{history[0]}
		recovery := CompactFunc(func(_ context.Context, _ []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
			return CompactionResult{History: recoveredHistory, Strategies: []string{"recovery"}, Changed: true}, nil
		})
		result, err := NewCompactorChain(failed, recovery).Compact(
			context.Background(),
			history,
			CompactionBudget{MaxRunes: estimateRunes(recoveredHistory)},
			nil,
			nil,
		)
		if err != nil {
			t.Fatalf("Compact() returned strategy error: %v", err)
		}
		if len(result.Errors) != 1 || !errors.Is(result.Errors[0], strategyErr) {
			t.Errorf("Compact() Errors = %#v, want strategy error", result.Errors)
		}
		if !reflect.DeepEqual(result.History, recoveredHistory) {
			t.Errorf("Compact() history = %#v, want recovery history", result.History)
		}
	})

	t.Run("budget exceeded is best effort not returned", func(t *testing.T) {
		t.Parallel()

		result, err := NewCompactorChain().Compact(
			context.Background(),
			history,
			CompactionBudget{MaxRunes: 0},
			nil,
			nil,
		)
		if err != nil {
			t.Fatalf("Compact() error = %v, want nil best-effort result", err)
		}
		if !reflect.DeepEqual(result.History, history) {
			t.Errorf("Compact() history = %#v, want unchanged best effort", result.History)
		}
	})

	t.Run("provider error is surfaced through chain errors", func(t *testing.T) {
		t.Parallel()

		providerErr := errors.New("provider failed")
		provider := &compactorProvider{err: providerErr}
		result, err := NewCompactorChain(NewSummarizationCompactor()).Compact(
			context.Background(),
			history,
			CompactionBudget{MaxRunes: 1000},
			provider,
			nil,
		)
		if err != nil {
			t.Fatalf("Compact() error = %v, want nil", err)
		}
		if len(result.Errors) != 1 || !errors.Is(result.Errors[0], providerErr) {
			t.Errorf("Compact() Errors = %#v, want provider error", result.Errors)
		}
	})
}

func TestCompactorCallerImmutability(t *testing.T) {
	t.Parallel()

	assistant, results := compactorToolExchange(compactorToolCall{id: "call", name: "read", result: strings.Repeat("content", 100)})
	history := []llm.Message{
		llm.SystemMessage("policy"), llm.UserMessage("first"), assistant, results[0], llm.UserMessage("latest"), llm.AssistantMessage("final"),
	}
	before, err := json.Marshal(history)
	if err != nil {
		t.Fatalf("json.Marshal(history): %v", err)
	}

	compactors := []struct {
		name      string
		compactor Compactor
		provider  llm.Provider
	}{
		{name: "drop oldest tool groups", compactor: NewDropOldestToolGroupsCompactor()},
		{name: "sliding window", compactor: NewSlidingWindowCompactor()},
		{name: "summarization", compactor: NewSummarizationCompactor(), provider: &compactorProvider{response: compactorMessagePointer(llm.AssistantMessage("summary"))}},
		{name: "standard chain", compactor: NewStandardCompactor()},
	}
	for _, tt := range compactors {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, _ = tt.compactor.Compact(context.Background(), history, CompactionBudget{MaxRunes: 0}, tt.provider, nil)
			after, marshalErr := json.Marshal(history)
			if marshalErr != nil {
				t.Fatalf("json.Marshal(history after Compact): %v", marshalErr)
			}
			if string(after) != string(before) {
				t.Errorf("Compact() mutated caller history:\nbefore %s\nafter  %s", before, after)
			}
		})
	}
}

func TestCompactorStandardSingleProtectedToolGroup(t *testing.T) {
	t.Parallel()

	assistant, results := compactorToolExchange(compactorToolCall{id: "call", name: "read", result: strings.Repeat("large", 100)})
	history := append([]llm.Message{llm.SystemMessage("policy"), llm.UserMessage("question"), assistant}, results...)
	result, err := NewStandardCompactor().Compact(
		context.Background(),
		history,
		CompactionBudget{MaxRunes: 0},
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("Compact() error = %v, want nil best effort", err)
	}
	if result.Changed || !reflect.DeepEqual(result.History, history) {
		t.Errorf("Compact() = %#v, want unchanged protected history", result)
	}
}

type compactorToolCall struct {
	id     string
	name   string
	result string
}

func compactorToolExchange(calls ...compactorToolCall) (llm.Message, []llm.Message) {
	toolCalls := make([]*llm.ToolUseBlock, 0, len(calls))
	results := make([]llm.Message, 0, len(calls))
	for _, call := range calls {
		toolCalls = append(toolCalls, &llm.ToolUseBlock{
			ID:    call.id,
			Name:  call.name,
			Input: json.RawMessage(`{"value":"test"}`),
		})
		results = append(results, compactorToolResult(call.id, call.result))
	}
	return llm.AssistantToolCallMessage(toolCalls...), results
}

func compactorToolResult(id string, content string) llm.Message {
	return llm.ToolResultMessage(id, tool.NewTextResult(content))
}

func compactorText(message llm.Message) string {
	var texts []string
	for _, block := range message.Content {
		if text, ok := block.(*llm.TextBlock); ok {
			texts = append(texts, text.Text)
		}
	}
	return strings.Join(texts, "\n")
}

func compactorMessagePointer(message llm.Message) *llm.Message {
	return &message
}

type compactorProvider struct {
	response *llm.Message
	err      error
	requests [][]llm.Message
}

func (p *compactorProvider) Name() string {
	return "compactor-test"
}

func (p *compactorProvider) Chat(
	_ context.Context,
	messages []llm.Message,
	_ []tool.ToolInfo,
	_ ...llm.Option,
) (*llm.Message, *llm.Usage, error) {
	request := append([]llm.Message{}, messages...)
	p.requests = append(p.requests, request)
	return p.response, nil, p.err
}

func (p *compactorProvider) ChatStream(
	_ context.Context,
	_ []llm.Message,
	_ []tool.ToolInfo,
	_ ...llm.Option,
) (iter.Seq2[llm.Chunk, error], error) {
	return nil, llm.ErrStreamingNotSupported
}

// Pins the issue #13 chain adoption contract: candidates are validated
// before adoption and before the budget early-exit, Changed=false is a
// no-op declaration, and an anomalous empty Changed=true candidate is
// recorded but never truncates the working history.
func TestCompactorChainCandidateValidation(t *testing.T) {
	t.Parallel()

	chainSeed := func() []llm.Message {
		base := []llm.Message{
			llm.SystemMessage("policy"),
			llm.UserMessage("first"),
		}
		assistant, results := compactorToolExchange(compactorToolCall{
			id: "c1", name: "chain_tool", result: strings.Repeat("file contents", 10),
		})
		base = append(base, assistant)
		base = append(base, results...)
		return append(base, llm.UserMessage("latest"))
	}

	validReduction := func() []llm.Message {
		return []llm.Message{
			llm.SystemMessage("policy"),
			llm.UserMessage("first"),
			llm.AssistantMessage("[Summary]\ncompact"),
			llm.UserMessage("latest"),
		}
	}

	t.Run("orphan-result candidate rejected and chain continues", func(t *testing.T) {
		t.Parallel()
		seed := chainSeed()
		budget := CompactionBudget{MaxRunes: estimateRunes(validReduction())}
		stage2Ran := false
		chain := NewCompactorChain(
			CompactFunc(func(_ context.Context, _ []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
				return CompactionResult{
					History: []llm.Message{
						llm.SystemMessage("policy"),
						llm.ToolResultMessage("orphan", tool.NewTextResult("no matching call")),
					},
					Changed: true,
				}, nil
			}),
			CompactFunc(func(_ context.Context, _ []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
				stage2Ran = true
				return CompactionResult{History: validReduction(), Changed: true}, nil
			}),
		)
		result, err := chain.Compact(context.Background(), seed, budget, nil, nil)
		if err != nil {
			t.Fatalf("Compact() error = %v", err)
		}
		if !stage2Ran {
			t.Fatalf("later stage skipped: invalid small candidate terminated the chain")
		}
		if len(result.Errors) != 1 || !strings.Contains(result.Errors[0].Error(), "invalid candidate") {
			t.Errorf("Errors = %#v, want one invalid-candidate diagnostic", result.Errors)
		}
		if !reflect.DeepEqual(result.History, validReduction()) {
			t.Errorf("history = %#v, want the valid stage-2 reduction", result.History)
		}
	})

	t.Run("system-prefix stripping candidate rejected", func(t *testing.T) {
		t.Parallel()
		seed := chainSeed()
		budget := CompactionBudget{MaxRunes: estimateRunes(validReduction())}
		stage2Ran := false
		chain := NewCompactorChain(
			CompactFunc(func(_ context.Context, _ []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
				return CompactionResult{History: []llm.Message{llm.UserMessage("latest")}, Changed: true}, nil
			}),
			CompactFunc(func(_ context.Context, _ []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
				stage2Ran = true
				return CompactionResult{History: validReduction(), Changed: true}, nil
			}),
		)
		result, err := chain.Compact(context.Background(), seed, budget, nil, nil)
		if err != nil {
			t.Fatalf("Compact() error = %v", err)
		}
		if !stage2Ran {
			t.Fatalf("later stage skipped: prefix-stripping candidate terminated the chain")
		}
		if len(result.Errors) != 1 || !strings.Contains(result.Errors[0].Error(), "system prefix") {
			t.Errorf("Errors = %#v, want one system-prefix diagnostic", result.Errors)
		}
		if !reflect.DeepEqual(result.History, validReduction()) {
			t.Errorf("history = %#v, want the valid stage-2 reduction", result.History)
		}
	})

	t.Run("zero-value stage skipped without diagnostics", func(t *testing.T) {
		t.Parallel()
		seed := chainSeed()
		final := validReduction()
		budget := CompactionBudget{MaxRunes: estimateRunes(final)}
		partial := append(copyMessages(final), llm.AssistantMessage("still over budget"))
		stage3Ran := false
		chain := NewCompactorChain(
			CompactFunc(func(_ context.Context, _ []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
				return CompactionResult{History: partial, Changed: true}, nil
			}),
			CompactFunc(func(_ context.Context, _ []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
				return CompactionResult{}, nil
			}),
			CompactFunc(func(_ context.Context, _ []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
				stage3Ran = true
				return CompactionResult{History: final, Changed: true}, nil
			}),
		)
		result, err := chain.Compact(context.Background(), seed, budget, nil, nil)
		if err != nil {
			t.Fatalf("Compact() error = %v", err)
		}
		if !stage3Ran {
			t.Fatalf("stage 3 skipped: zero-value candidate truncated the working history")
		}
		if len(result.Errors) != 0 {
			t.Errorf("Errors = %#v, want none (zero-value is a documented no-op)", result.Errors)
		}
		if !reflect.DeepEqual(result.History, final) {
			t.Errorf("history = %#v, want the stage-3 reduction", result.History)
		}
	})

	t.Run("empty with Changed=true yields diagnostic", func(t *testing.T) {
		t.Parallel()
		seed := chainSeed()
		final := validReduction()
		budget := CompactionBudget{MaxRunes: estimateRunes(final)}
		stage2Ran := false
		chain := NewCompactorChain(
			CompactFunc(func(_ context.Context, _ []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
				return CompactionResult{Changed: true}, nil
			}),
			CompactFunc(func(_ context.Context, _ []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
				stage2Ran = true
				return CompactionResult{History: final, Changed: true}, nil
			}),
		)
		result, err := chain.Compact(context.Background(), seed, budget, nil, nil)
		if err != nil {
			t.Fatalf("Compact() error = %v", err)
		}
		if !stage2Ran {
			t.Fatalf("stage 2 skipped: empty Changed=true candidate truncated the working history")
		}
		if len(result.Errors) != 1 || !strings.Contains(result.Errors[0].Error(), "empty history") {
			t.Errorf("Errors = %#v, want one empty-history diagnostic", result.Errors)
		}
		if !reflect.DeepEqual(result.History, final) {
			t.Errorf("history = %#v, want the stage-2 reduction", result.History)
		}
	})

	t.Run("Changed=false smaller candidate never replaces", func(t *testing.T) {
		t.Parallel()
		seed := chainSeed()
		budget := CompactionBudget{MaxRunes: estimateRunes(seed) - 1}
		chain := NewCompactorChain(
			CompactFunc(func(_ context.Context, _ []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
				return CompactionResult{History: validReduction(), Changed: false}, nil
			}),
		)
		result, err := chain.Compact(context.Background(), seed, budget, nil, nil)
		if err != nil {
			t.Fatalf("Compact() error = %v", err)
		}
		if result.Changed {
			t.Errorf("Changed = true, want false (Changed=false never replaces)")
		}
		if !reflect.DeepEqual(result.History, copyMessages(seed)) {
			t.Errorf("history was replaced by a Changed=false candidate")
		}
		if len(result.Errors) != 0 {
			t.Errorf("Errors = %#v, want none", result.Errors)
		}
	})

	t.Run("foreign-block candidate rejected and chain continues", func(t *testing.T) {
		t.Parallel()
		seed := chainSeed()
		budget := CompactionBudget{MaxRunes: estimateRunes(validReduction())}
		stage2Ran := false
		chain := NewCompactorChain(
			CompactFunc(func(_ context.Context, _ []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
				return CompactionResult{
					History: []llm.Message{
						llm.SystemMessage("policy"),
						{Role: llm.RoleUser, Content: []llm.ContentBlock{
							wrappedTextBlock{&llm.TextBlock{Type: "text", Text: "foreign"}},
						}},
					},
					Changed: true,
				}, nil
			}),
			CompactFunc(func(_ context.Context, _ []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
				stage2Ran = true
				return CompactionResult{History: validReduction(), Changed: true}, nil
			}),
		)
		result, err := chain.Compact(context.Background(), seed, budget, nil, nil)
		if err != nil {
			t.Fatalf("Compact() error = %v", err)
		}
		if !stage2Ran {
			t.Fatalf("later stage skipped: foreign-block candidate terminated the chain")
		}
		if len(result.Errors) != 1 || !strings.Contains(result.Errors[0].Error(), "clone whitelist") {
			t.Errorf("Errors = %#v, want one clone-whitelist diagnostic", result.Errors)
		}
		if !reflect.DeepEqual(result.History, validReduction()) {
			t.Errorf("history = %#v, want the valid stage-2 reduction", result.History)
		}
	})

	t.Run("accepted candidate is deep-copied at adoption", func(t *testing.T) {
		t.Parallel()
		seed := []llm.Message{
			{Role: llm.RoleSystem, Content: []llm.ContentBlock{&llm.TextBlock{Type: "text", Text: "policy"}}},
			llm.UserMessage("first"),
		}
		assistant, results := compactorToolExchange(compactorToolCall{
			id: "c1", name: "chain_tool", result: strings.Repeat("file contents", 10),
		})
		seed = append(seed, assistant)
		seed = append(seed, results...)
		seed = append(seed, llm.UserMessage("latest"))
		candidate := []llm.Message{
			llm.SystemMessage("policy"),
			llm.UserMessage("latest"),
		}
		budget := CompactionBudget{MaxRunes: estimateRunes(candidate)}
		var retained *llm.TextBlock
		chain := NewCompactorChain(
			CompactFunc(func(_ context.Context, h []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
				sysBlock := h[0].Content[0].(*llm.TextBlock)
				retained = sysBlock
				return CompactionResult{
					History: []llm.Message{
						{Role: llm.RoleSystem, Content: []llm.ContentBlock{sysBlock}},
						llm.UserMessage("latest"),
					},
					Changed: true,
				}, nil
			}),
		)
		result, err := chain.Compact(context.Background(), seed, budget, nil, nil)
		if err != nil {
			t.Fatalf("Compact() error = %v", err)
		}
		retained.Text = "MUTATED-AFTER"
		if got := result.History[0].Content[0].(*llm.TextBlock).Text; got != "policy" {
			t.Errorf("accepted history mutated through the stage's retained pointer: %q", got)
		}
	})
}

type nonComparableError []string

func (e nonComparableError) Error() string { return strings.Join(e, ";") }

type mustNotCallProvider struct{ called bool }

func (p *mustNotCallProvider) Chat(_ context.Context, _ []llm.Message, _ []tool.ToolInfo, _ ...llm.Option) (*llm.Message, *llm.Usage, error) {
	p.called = true
	return nil, nil, errors.New("provider called after cancellation")
}

func (p *mustNotCallProvider) ChatStream(_ context.Context, _ []llm.Message, _ []tool.ToolInfo, _ ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	p.called = true
	return nil, errors.New("provider called after cancellation")
}

func (p *mustNotCallProvider) Name() string { return "must-not-call" }

func TestCompactorChainCancellation(t *testing.T) {
	t.Parallel()

	chainSeed := func() []llm.Message {
		return []llm.Message{
			llm.SystemMessage("policy"),
			llm.UserMessage("first"),
			llm.AssistantMessage(strings.Repeat("payload", 40)),
			llm.UserMessage("latest"),
		}
	}
	reduction := func() []llm.Message {
		return []llm.Message{llm.SystemMessage("policy"), llm.UserMessage("latest")}
	}

	t.Run("canceled context skips later stages", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		seed := chainSeed()
		budget := CompactionBudget{MaxRunes: estimateRunes(seed) - 1}
		stage2Ran := false
		chain := NewCompactorChain(
			CompactFunc(func(_ context.Context, _ []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
				return CompactionResult{History: reduction(), Changed: true}, nil
			}),
			CompactFunc(func(_ context.Context, _ []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
				stage2Ran = true
				return CompactionResult{}, nil
			}),
		)
		result, err := chain.Compact(ctx, seed, budget, nil, nil)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Compact() error = %v, want context.Canceled", err)
		}
		if stage2Ran {
			t.Fatalf("stage 2 invoked after cancellation")
		}
		if len(result.Errors) != 1 || !errors.Is(result.Errors[0], context.Canceled) {
			t.Errorf("Errors = %#v, want exactly one context.Canceled", result.Errors)
		}
	})

	t.Run("context dying during a stage stops the chain after it returns", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		seed := chainSeed()
		budget := CompactionBudget{MaxRunes: estimateRunes(seed) - 1}
		stage2Ran := false
		chain := NewCompactorChain(
			CompactFunc(func(_ context.Context, _ []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
				cancel() // stage completes normally while its context dies underneath it
				return CompactionResult{}, nil
			}),
			CompactFunc(func(_ context.Context, _ []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
				stage2Ran = true
				return CompactionResult{}, nil
			}),
		)
		_, err := chain.Compact(ctx, seed, budget, nil, nil)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Compact() error = %v, want context.Canceled", err)
		}
		if stage2Ran {
			t.Fatalf("stage 2 invoked after the context died mid-stage")
		}
	})

	t.Run("stage-reported cancellation with a live context does not abort the chain", func(t *testing.T) {
		t.Parallel()
		seed := chainSeed()
		budget := CompactionBudget{MaxRunes: estimateRunes(reduction())}
		stage2Ran := false
		chain := NewCompactorChain(
			CompactFunc(func(_ context.Context, _ []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
				return CompactionResult{}, context.Canceled // e.g. the stage's own internal timeout fired
			}),
			CompactFunc(func(_ context.Context, _ []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
				stage2Ran = true
				return CompactionResult{History: reduction(), Changed: true}, nil
			}),
		)
		result, err := chain.Compact(context.Background(), seed, budget, nil, nil)
		if err != nil {
			t.Fatalf("Compact() error = %v, want nil", err)
		}
		if !stage2Ran {
			t.Fatalf("stage 2 skipped: spurious stage cancellation aborted the chain")
		}
		if len(result.Errors) != 1 {
			t.Errorf("Errors = %#v, want the spurious cancellation recorded once", result.Errors)
		}
		if !result.Changed {
			t.Errorf("Changed = false, want the stage-2 reduction adopted")
		}
	})

	t.Run("nested chain cancellation recorded exactly once", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		seed := chainSeed()
		budget := CompactionBudget{MaxRunes: estimateRunes(seed) - 1}
		outerStage2Ran := false
		inner := NewCompactorChain(
			CompactFunc(func(_ context.Context, _ []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
				cancel() // the caller's context dies inside the nested stage
				// The nested chain's own boundary check records the caller's
				// cancellation and returns it; RecordedContextCancellation and
				// ReturnedError are what the inner chain machinery sets.
				return CompactionResult{Errors: []error{ctx.Err()}, RecordedContextCancellation: true, ReturnedError: ctx.Err()}, ctx.Err()
			}),
		)
		outer := NewCompactorChain(
			inner,
			CompactFunc(func(_ context.Context, _ []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
				outerStage2Ran = true
				return CompactionResult{}, nil
			}),
		)
		result, err := outer.Compact(ctx, seed, budget, nil, nil)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Compact() error = %v, want context.Canceled", err)
		}
		if outerStage2Ran {
			t.Fatalf("outer stage 2 invoked after cancellation")
		}
		if len(result.Errors) != 1 || !errors.Is(result.Errors[0], context.Canceled) {
			t.Errorf("Errors = %#v, want context.Canceled recorded exactly once", result.Errors)
		}
	})

	t.Run("caller's cancellation outranks a stage-reported deadline error", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		seed := chainSeed()
		budget := CompactionBudget{MaxRunes: estimateRunes(seed) - 1}
		stage2Ran := false
		chain := NewCompactorChain(
			CompactFunc(func(_ context.Context, _ []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
				return CompactionResult{}, context.DeadlineExceeded // the stage's own private timeout fired
			}),
			CompactFunc(func(_ context.Context, _ []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
				stage2Ran = true
				return CompactionResult{}, nil
			}),
		)
		result, err := chain.Compact(ctx, seed, budget, nil, nil)
		if !errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Compact() error = %v, want the caller's context.Canceled", err)
		}
		if stage2Ran {
			t.Fatalf("stage 2 invoked after cancellation")
		}
		if len(result.Errors) != 1 || !errors.Is(result.Errors[0], context.Canceled) {
			t.Errorf("Errors = %#v, want only the caller's cancellation recorded", result.Errors)
		}
	})
}

func TestCompactorChainMergesCandidateErrors(t *testing.T) {
	t.Parallel()

	chainSeed := func() []llm.Message {
		return []llm.Message{
			llm.SystemMessage("policy"),
			llm.UserMessage("first"),
			llm.AssistantMessage(strings.Repeat("payload", 40)),
			llm.UserMessage("latest"),
		}
	}
	reduction := func() []llm.Message {
		return []llm.Message{llm.SystemMessage("policy"), llm.UserMessage("latest")}
	}

	t.Run("nested chain diagnostics surface", func(t *testing.T) {
		t.Parallel()
		seed := chainSeed()
		budget := CompactionBudget{MaxRunes: estimateRunes(reduction())}
		innerDiag := errors.New("inner summarizer failed; fallback succeeded")
		inner := NewCompactorChain(
			CompactFunc(func(_ context.Context, _ []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
				return CompactionResult{}, innerDiag
			}),
			CompactFunc(func(_ context.Context, _ []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
				return CompactionResult{History: reduction(), Changed: true}, nil
			}),
		)
		outer := NewCompactorChain(
			inner,
			CompactFunc(func(_ context.Context, _ []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
				return CompactionResult{}, nil
			}),
		)
		result, err := outer.Compact(context.Background(), seed, budget, nil, nil)
		if err != nil {
			t.Fatalf("Compact() error = %v", err)
		}
		if len(result.Errors) != 1 || !errors.Is(result.Errors[0], innerDiag) {
			t.Errorf("Errors = %#v, want the inner chain's diagnostic surfaced once", result.Errors)
		}
		if !result.Changed {
			t.Errorf("Changed = false, want the inner reduction adopted")
		}
	})

	t.Run("undeclared returned error is kept alongside the candidate's entry", func(t *testing.T) {
		t.Parallel()
		seed := chainSeed()
		budget := CompactionBudget{MaxRunes: estimateRunes(reduction())}
		diag := errors.New("stage diagnostic")
		chain := NewCompactorChain(
			CompactFunc(func(_ context.Context, _ []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
				// The stage records the diagnostic AND returns the same error
				// without declaring ReturnedError: identity with a candidate
				// entry proves nothing (two private attempts can share the
				// sentinel), so both entries stay.
				return CompactionResult{Errors: []error{diag}}, diag
			}),
		)
		result, err := chain.Compact(context.Background(), seed, budget, nil, nil)
		if err != nil {
			t.Fatalf("Compact() error = %v, want nil (non-cancellation stage errors never become the chain's error)", err)
		}
		if len(result.Errors) != 2 || result.Errors[0] != diag || result.Errors[1] != diag {
			t.Errorf("Errors = %#v, want the undeclared returned error kept alongside the candidate entry", result.Errors)
		}
	})

	t.Run("declared returned error is not recorded twice", func(t *testing.T) {
		t.Parallel()
		seed := chainSeed()
		budget := CompactionBudget{MaxRunes: estimateRunes(reduction())}
		diag := errors.New("stage diagnostic")
		chain := NewCompactorChain(
			CompactFunc(func(_ context.Context, _ []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
				// The stage declares diag as its returned error (what chain
				// machinery does): the event is already recorded once.
				return CompactionResult{Errors: []error{diag}, ReturnedError: diag}, diag
			}),
		)
		result, err := chain.Compact(context.Background(), seed, budget, nil, nil)
		if err != nil {
			t.Fatalf("Compact() error = %v, want nil", err)
		}
		if len(result.Errors) != 1 || result.Errors[0] != diag {
			t.Errorf("Errors = %#v, want the declared returned error recorded exactly once", result.Errors)
		}
	})

	t.Run("distinct same-sentinel stage failures are both recorded", func(t *testing.T) {
		t.Parallel()
		seed := chainSeed()
		budget := CompactionBudget{MaxRunes: estimateRunes(seed) - 1}
		chain := NewCompactorChain(
			CompactFunc(func(_ context.Context, _ []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
				return CompactionResult{}, context.DeadlineExceeded // stage 1's private timeout
			}),
			CompactFunc(func(_ context.Context, _ []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
				return CompactionResult{}, context.DeadlineExceeded // stage 2's private timeout
			}),
		)
		result, err := chain.Compact(context.Background(), seed, budget, nil, nil)
		if err != nil {
			t.Fatalf("Compact() error = %v, want nil", err)
		}
		if len(result.Errors) != 2 {
			t.Errorf("Errors = %#v, want both distinct private timeout failures recorded", result.Errors)
		}
	})

	t.Run("joined error's independent cause survives caller cancellation", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		seed := chainSeed()
		budget := CompactionBudget{MaxRunes: estimateRunes(seed) - 1}
		stageErr := errors.Join(context.Canceled, errors.New("provider failed"))
		chain := NewCompactorChain(
			CompactFunc(func(_ context.Context, _ []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
				cancel() // the caller cancels while the stage fails with a joined error
				return CompactionResult{}, stageErr
			}),
		)
		result, err := chain.Compact(ctx, seed, budget, nil, nil)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Compact() error = %v, want context.Canceled", err)
		}
		if len(result.Errors) != 2 {
			t.Errorf("Errors = %#v, want the joined stage error and the caller cancellation both recorded", result.Errors)
		}
	})

	t.Run("wrapped private timeout returned by a successful stage does not suppress caller cancellation", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		seed := chainSeed()
		budget := CompactionBudget{MaxRunes: estimateRunes(seed) - 1}
		wrapped := fmt.Errorf("private timeout: %w", context.DeadlineExceeded)
		chain := NewCompactorChain(
			CompactFunc(func(_ context.Context, _ []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
				cancel() // caller deadline expires while the stage records its own wrapped private timeout
				return CompactionResult{Errors: []error{wrapped}}, wrapped
			}),
		)
		result, err := chain.Compact(ctx, seed, budget, nil, nil)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Compact() error = %v, want context.Canceled", err)
		}
		if len(result.Errors) != 2 || !errors.Is(result.Errors[0], context.DeadlineExceeded) || !errors.Is(result.Errors[1], context.Canceled) {
			t.Errorf("Errors = %#v, want [wrapped private timeout, context.Canceled]", result.Errors)
		}
	})

	t.Run("non-comparable error type does not panic identity comparison", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		seed := chainSeed()
		budget := CompactionBudget{MaxRunes: estimateRunes(seed) - 1}
		chain := NewCompactorChain(
			CompactFunc(func(_ context.Context, _ []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
				cancel()
				return CompactionResult{Errors: []error{nonComparableError{"private", "timeout"}}}, nonComparableError{"private", "timeout"}
			}),
		)
		result, err := chain.Compact(ctx, seed, budget, nil, nil)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Compact() error = %v, want context.Canceled", err)
		}
		// Non-comparable errors cannot be tested for identity (the interface
		// comparison would panic), so per the keep-both-diagnostics rule the
		// merged candidate entry, the returned error, and the caller's
		// cancellation are all retained — nothing is dropped, no panic.
		if len(result.Errors) != 3 || !errors.Is(result.Errors[2], context.Canceled) {
			t.Errorf("Errors = %#v, want both non-comparable diagnostics and context.Canceled kept", result.Errors)
		}
	})

	t.Run("private same-sentinel diagnostic does not suppress caller's own cancellation", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		seed := chainSeed()
		budget := CompactionBudget{MaxRunes: estimateRunes(seed) - 1}
		chain := NewCompactorChain(
			CompactFunc(func(_ context.Context, _ []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
				cancel() // caller deadline expires during a fallback attempt
				// The stage recorded its own private DeadlineExceeded (a
				// diagnostic for its internal attempt) and then returned the
				// caller's ctx.Err() from the fallback path: two distinct
				// events carrying the same sentinel.
				return CompactionResult{Errors: []error{context.DeadlineExceeded}}, ctx.Err()
			}),
		)
		result, err := chain.Compact(ctx, seed, budget, nil, nil)
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Compact() error = %v, want the caller's context error", err)
		}
		if len(result.Errors) != 2 {
			t.Errorf("Errors = %#v, want the stage's private diagnostic and the caller cancellation both recorded", result.Errors)
		}
	})

	t.Run("two bare-sentinel private attempts are both recorded", func(t *testing.T) {
		t.Parallel()
		seed := chainSeed()
		budget := CompactionBudget{MaxRunes: estimateRunes(seed) - 1}
		chain := NewCompactorChain(
			CompactFunc(func(_ context.Context, _ []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
				// Private attempt 1 fails with the sentinel, recorded but not
				// returned (the stage retries internally).
				return CompactionResult{Errors: []error{context.DeadlineExceeded}}, nil
			}),
			CompactFunc(func(_ context.Context, _ []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
				// Private attempt 2 fails with the same singleton sentinel and
				// the stage returns it. No ReturnedError declaration: both
				// attempts stay recorded.
				return CompactionResult{}, context.DeadlineExceeded
			}),
		)
		result, err := chain.Compact(context.Background(), seed, budget, nil, nil)
		if err != nil {
			t.Fatalf("Compact() error = %v, want nil", err)
		}
		if len(result.Errors) != 2 {
			t.Errorf("Errors = %#v, want both private attempts recorded", result.Errors)
		}
	})

	t.Run("caller cancellation during a failed stage keeps both diagnostics", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		seed := chainSeed()
		budget := CompactionBudget{MaxRunes: estimateRunes(seed) - 1}
		providerErr := errors.New("provider failed")
		chain := NewCompactorChain(
			CompactFunc(func(_ context.Context, _ []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
				cancel() // the caller cancels while the stage is failing
				return CompactionResult{}, providerErr
			}),
		)
		result, err := chain.Compact(ctx, seed, budget, nil, nil)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Compact() error = %v, want context.Canceled", err)
		}
		if len(result.Errors) != 2 || !errors.Is(result.Errors[0], providerErr) || !errors.Is(result.Errors[1], context.Canceled) {
			t.Errorf("Errors = %#v, want [providerErr, context.Canceled]", result.Errors)
		}
	})

	t.Run("successful stage's private timeout does not stand in for caller's cancellation", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		seed := chainSeed()
		budget := CompactionBudget{MaxRunes: estimateRunes(seed) - 1}
		chain := NewCompactorChain(
			CompactFunc(func(_ context.Context, _ []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
				cancel() // caller deadline expires while the stage succeeds
				// The stage recorded its own private timeout as a diagnostic
				// and returned nil: two distinct events.
				return CompactionResult{Errors: []error{context.DeadlineExceeded}}, nil
			}),
		)
		result, err := chain.Compact(ctx, seed, budget, nil, nil)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Compact() error = %v, want context.Canceled", err)
		}
		if len(result.Errors) != 2 || !errors.Is(result.Errors[0], context.DeadlineExceeded) || !errors.Is(result.Errors[1], context.Canceled) {
			t.Errorf("Errors = %#v, want [private timeout, context.Canceled]", result.Errors)
		}
	})
}

func TestSummarizationCompactorChecksContextBeforeChat(t *testing.T) {
	t.Parallel()

	assistant, results := compactorToolExchange(compactorToolCall{id: "c1", name: "read", result: strings.Repeat("content", 50)})
	history := []llm.Message{
		llm.SystemMessage("policy"), llm.UserMessage("first"), assistant, results[0], llm.UserMessage("latest"),
	}
	provider := &mustNotCallProvider{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := NewSummarizationCompactor().Compact(ctx, history, CompactionBudget{MaxRunes: 1}, provider, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Compact() error = %v, want context.Canceled", err)
	}
	if provider.called {
		t.Errorf("provider invoked after cancellation")
	}
}

func TestCompactorChainWithinBudgetCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// History fits the budget: the chain takes the within-budget early
	// return, which must still report cancellation (issue #14).
	seed := []llm.Message{llm.SystemMessage("policy"), llm.UserMessage("latest")}
	stageRan := false
	chain := NewCompactorChain(
		CompactFunc(func(_ context.Context, _ []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
			stageRan = true
			return CompactionResult{}, nil
		}),
	)
	result, err := chain.Compact(ctx, seed, CompactionBudget{MaxRunes: estimateRunes(seed)}, nil, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Compact() error = %v, want context.Canceled", err)
	}
	if stageRan {
		t.Errorf("stage invoked despite cancellation")
	}
	if len(result.Errors) != 1 || !errors.Is(result.Errors[0], context.Canceled) {
		t.Errorf("Errors = %#v, want exactly one context.Canceled", result.Errors)
	}
}

// Non-parallel: mutates the package-level compactionAdoptionHook seam.
func TestCompactorChainCancellationDuringAdoption(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	seed := []llm.Message{
		llm.SystemMessage("policy"),
		llm.UserMessage("first"),
		llm.AssistantMessage(strings.Repeat("payload", 40)),
		llm.UserMessage("latest"),
	}
	budget := CompactionBudget{MaxRunes: estimateRunes(seed) - 1}
	chain := NewCompactorChain(
		CompactFunc(func(_ context.Context, _ []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
			return CompactionResult{History: []llm.Message{llm.SystemMessage("policy"), llm.UserMessage("latest")}, Changed: true}, nil
		}),
	)
	compactionAdoptionHook = cancel
	defer func() { compactionAdoptionHook = nil }()

	_, err := chain.Compact(ctx, seed, budget, nil, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Compact() error = %v, want context.Canceled", err)
	}
	// result's other fields are unspecified once err != nil: both the kernel
	// (issue #9/#10 contract) and direct callers discard the result on error,
	// so whether the gate completed its in-place adoption before the final
	// recheck observed the cancellation is immaterial. What the final recheck
	// pins is that cancellation inside the gate takes the cancellation path
	// instead of a successful return.
}
