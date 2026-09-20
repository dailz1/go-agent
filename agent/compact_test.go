package agent

import (
	"context"
	"encoding/json"
	"errors"
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
			llm.TextBlock{Type: "text", Text: "I will inspect both files."},
			llm.ToolUseBlock{Type: "tool_use", ID: "hybrid", Name: "read", Input: json.RawMessage(`{"path":"a"}`)},
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
				llm.AssistantToolCallMessage(llm.ToolUseBlock{ID: "", Name: "read", Input: json.RawMessage(`{}`)}),
			},
			expectedErr: ErrInvalidHistory,
		},
		{
			name: "duplicate tool use id",
			history: []llm.Message{
				llm.UserMessage("question"),
				llm.AssistantToolCallMessage(
					llm.ToolUseBlock{ID: "same", Name: "read", Input: json.RawMessage(`{}`)},
					llm.ToolUseBlock{ID: "same", Name: "write", Input: json.RawMessage(`{}`)},
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
		llm.AssistantToolCallMessage(llm.ToolUseBlock{
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
				llm.TextBlock{Type: "text", Text: "中文😀"},
				llm.ToolUseBlock{Type: "tool_use", ID: "bad", Name: "lookup", Input: invalidInput},
				llm.ToolResultBlock{Type: "tool_result", ToolUseID: "bad", Content: "result"},
				llm.ReasoningBlock{Type: "reasoning", Content: "reason"},
				llm.ImageBlock{Type: "image", Data: []byte{1, 2, 3}},
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
			strategy := CompactFunc(func(_ context.Context, input []llm.Message, _ CompactionBudget) (CompactionResult, error) {
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

		assistant := llm.AssistantToolCallMessage(llm.ToolUseBlock{
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
		)
		budget = estimateRunes(first.History)
		second, err := NewDropOldestToolGroupsCompactor().Compact(
			context.Background(),
			first.History,
			CompactionBudget{MaxRunes: budget},
		)
		if err != nil {
			t.Fatalf("second Compact() unexpected error: %v", err)
		}
		if second.Changed || !reflect.DeepEqual(second.History, first.History) {
			t.Errorf("second Compact() = %#v, want unchanged", second)
		}
	})
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
		llm.AssistantMessage("more history"),
		llm.UserMessage("latest"),
		llm.AssistantMessage("current round"),
	}

	t.Run("success replaces entire span and preserves request systems", func(t *testing.T) {
		t.Parallel()

		provider := &compactorProvider{response: &llm.Message{
			Role: llm.RoleTool,
			Content: []llm.ContentBlock{
				llm.TextBlock{Type: "text", Text: "summary one"},
				llm.ReasoningBlock{Type: "reasoning", Content: "ignored"},
				llm.TextBlock{Type: "text", Text: "summary two"},
			},
		}}
		result, err := NewSummarizationCompactor(provider).Compact(
			context.Background(),
			history,
			CompactionBudget{MaxRunes: 0},
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
					llm.ReasoningBlock{Type: "reasoning", Content: "not summary text"},
				},
			}},
		},
		{name: "provider error", provider: &compactorProvider{err: errors.New("provider unavailable")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result, err := NewSummarizationCompactor(tt.provider).Compact(
				context.Background(),
				history,
				CompactionBudget{MaxRunes: 0},
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
		result, err := NewSummarizationCompactor(provider).Compact(
			context.Background(),
			shortHistory,
			CompactionBudget{MaxRunes: 0},
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

	history := []llm.Message{llm.UserMessage("first"), llm.AssistantMessage(strings.Repeat("history", 20)), llm.UserMessage("latest"), llm.AssistantMessage("final")}

	t.Run("runs in order and stops when caller budget is reached", func(t *testing.T) {
		t.Parallel()

		calls := []string{}
		first := CompactFunc(func(_ context.Context, input []llm.Message, _ CompactionBudget) (CompactionResult, error) {
			calls = append(calls, "drop")
			return CompactionResult{History: append([]llm.Message{}, input...), Strategies: []string{"drop"}}, nil
		})
		secondHistory := []llm.Message{history[0], history[2], history[3]}
		second := CompactFunc(func(_ context.Context, _ []llm.Message, _ CompactionBudget) (CompactionResult, error) {
			calls = append(calls, "window")
			return CompactionResult{History: secondHistory, Strategies: []string{"window"}, DroppedGroups: 1, Changed: true}, nil
		})
		third := CompactFunc(func(_ context.Context, input []llm.Message, _ CompactionBudget) (CompactionResult, error) {
			calls = append(calls, "third")
			return CompactionResult{History: input}, nil
		})
		result, err := NewCompactorChain(first, second, third).Compact(
			context.Background(),
			history,
			CompactionBudget{MaxRunes: estimateRunes(secondHistory)},
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
		failed := CompactFunc(func(_ context.Context, input []llm.Message, _ CompactionBudget) (CompactionResult, error) {
			return CompactionResult{History: input, Strategies: []string{"failed"}}, strategyErr
		})
		recoveredHistory := []llm.Message{history[0]}
		recovery := CompactFunc(func(_ context.Context, _ []llm.Message, _ CompactionBudget) (CompactionResult, error) {
			return CompactionResult{History: recoveredHistory, Strategies: []string{"recovery"}, Changed: true}, nil
		})
		result, err := NewCompactorChain(failed, recovery).Compact(
			context.Background(),
			history,
			CompactionBudget{MaxRunes: estimateRunes(recoveredHistory)},
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
		result, err := NewCompactorChain(NewSummarizationCompactor(provider)).Compact(
			context.Background(),
			history,
			CompactionBudget{MaxRunes: 0},
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
	}{
		{name: "drop oldest tool groups", compactor: NewDropOldestToolGroupsCompactor()},
		{name: "sliding window", compactor: NewSlidingWindowCompactor()},
		{name: "summarization", compactor: NewSummarizationCompactor(&compactorProvider{response: compactorMessagePointer(llm.AssistantMessage("summary"))})},
		{name: "standard chain", compactor: NewStandardCompactor()},
	}
	for _, tt := range compactors {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, _ = tt.compactor.Compact(context.Background(), history, CompactionBudget{MaxRunes: 0})
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
	toolCalls := make([]llm.ToolUseBlock, 0, len(calls))
	results := make([]llm.Message, 0, len(calls))
	for _, call := range calls {
		toolCalls = append(toolCalls, llm.ToolUseBlock{
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
		if text, ok := block.(llm.TextBlock); ok {
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
