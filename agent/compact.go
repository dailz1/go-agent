package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/dailz1/go-agent/llm"
)

var (
	// ErrInvalidHistory indicates that message roles or tool calls do not form a valid history.
	ErrInvalidHistory = errors.New("agent: invalid message history")
	// ErrCompactionBudgetExceeded indicates that a strategy exhausted its options before meeting the budget.
	ErrCompactionBudgetExceeded = errors.New("agent: compaction could not meet history budget")
)

const (
	defaultSummarizationPrompt = "You are summarizing part of an agent conversation so this summary can replace it: the original messages will be dropped, and future turns will rely on this summary alone. Preserve: the goal, key decisions, constraints and preferences, current progress, next steps, tool-call outcomes and their significance, and critical data verbatim (file paths, function names, commands, error messages). Omit pleasantries and redundant exchanges. Be factual and brief. Write the summary in the same language as the conversation. Summarize the following conversation:"
	maxToolCollapseRunes       = 4096
	toolCollapsePrefix         = "[Tool Calls]\n"
	toolCollapseSuffix         = "... [truncated]"
)

// CompactionBudget defines the maximum estimated history size.
type CompactionBudget struct {
	MaxRunes int
}

// CompactionResult describes a compaction attempt and any degraded strategy errors.
type CompactionResult struct {
	History       []llm.Message
	DroppedGroups int
	Strategies    []string
	Changed       bool
	Errors        []error
}

// Compactor reduces message history while preserving valid message groups.
type Compactor interface {
	Compact(ctx context.Context, history []llm.Message, budget CompactionBudget) (CompactionResult, error)
}

// CompactFunc adapts a function to Compactor.
type CompactFunc func(
	ctx context.Context,
	history []llm.Message,
	budget CompactionBudget,
) (CompactionResult, error)

// Compact delegates to f.
func (f CompactFunc) Compact(
	ctx context.Context,
	history []llm.Message,
	budget CompactionBudget,
) (CompactionResult, error) {
	return f(ctx, history, budget)
}

type historyGroup struct {
	messages []llm.Message
	isSystem bool
	isTool   bool
}

func partitionHistory(history []llm.Message) ([]historyGroup, error) {
	groups := []historyGroup{}
	index := 0
	for index < len(history) && history[index].Role == llm.RoleSystem {
		index++
	}
	if index > 0 {
		groups = append(groups, historyGroup{
			messages: copyMessages(history[:index]),
			isSystem: true,
		})
	}

	for index < len(history) {
		message := history[index]
		switch message.Role {
		case llm.RoleSystem:
			return nil, invalidHistoryf("system message at index %d appears after the system prefix", index)
		case llm.RoleUser:
			groups = append(groups, historyGroup{messages: []llm.Message{message}})
			index++
		case llm.RoleAssistant:
			toolUseIDs, hasToolUse, err := assistantToolUseIDs(message)
			if err != nil {
				return nil, fmt.Errorf("assistant message at index %d: %w", index, err)
			}
			if !hasToolUse {
				groups = append(groups, historyGroup{messages: []llm.Message{message}})
				index++
				continue
			}

			end := index + 1
			for end < len(history) && history[end].Role == llm.RoleTool {
				end++
			}
			if err := validateToolResults(history[index+1:end], toolUseIDs); err != nil {
				return nil, fmt.Errorf("tool group at index %d: %w", index, err)
			}
			groups = append(groups, historyGroup{
				messages: copyMessages(history[index:end]),
				isTool:   true,
			})
			index = end
		case llm.RoleTool:
			return nil, invalidHistoryf("orphan tool result at index %d", index)
		default:
			return nil, invalidHistoryf("unknown role %q at index %d", message.Role, index)
		}
	}

	return groups, nil
}

func assistantToolUseIDs(message llm.Message) (map[string]struct{}, bool, error) {
	ids := map[string]struct{}{}
	hasToolUse := false
	for _, block := range message.Content {
		var id string
		switch typed := block.(type) {
		case llm.ToolUseBlock:
			id = typed.ID
		case *llm.ToolUseBlock:
			if typed == nil {
				continue
			}
			id = typed.ID
		default:
			continue
		}

		hasToolUse = true
		if id == "" {
			return nil, false, invalidHistoryf("tool use id is empty")
		}
		if _, exists := ids[id]; exists {
			return nil, false, invalidHistoryf("tool use id %q is duplicated", id)
		}
		ids[id] = struct{}{}
	}
	return ids, hasToolUse, nil
}

func validateToolResults(messages []llm.Message, expected map[string]struct{}) error {
	seen := map[string]struct{}{}
	for _, message := range messages {
		foundResult := false
		for _, block := range message.Content {
			var id string
			switch typed := block.(type) {
			case llm.ToolResultBlock:
				id = typed.ToolUseID
			case *llm.ToolResultBlock:
				if typed == nil {
					continue
				}
				id = typed.ToolUseID
			default:
				continue
			}

			foundResult = true
			if _, exists := expected[id]; !exists {
				return invalidHistoryf("tool result id %q is unknown", id)
			}
			if _, exists := seen[id]; exists {
				return invalidHistoryf("tool result id %q is duplicated", id)
			}
			seen[id] = struct{}{}
		}
		if !foundResult {
			return invalidHistoryf("tool result message has no result block")
		}
	}
	for id := range expected {
		if _, exists := seen[id]; !exists {
			return invalidHistoryf("tool result id %q is missing", id)
		}
	}
	return nil
}

func invalidHistoryf(format string, args ...any) error {
	detail := fmt.Sprintf(format, args...)
	return fmt.Errorf("%s: %w", detail, ErrInvalidHistory)
}

func estimateRunes(history []llm.Message) int {
	encoded, err := json.Marshal(history)
	if err == nil {
		return utf8.RuneCount(encoded)
	}

	total := 0
	for _, message := range history {
		total += len(message.Role)
		for _, block := range message.Content {
			total += fallbackBlockSize(block)
		}
	}
	return total
}

func fallbackBlockSize(block llm.ContentBlock) int {
	switch typed := block.(type) {
	case llm.TextBlock:
		return len(typed.Text)
	case *llm.TextBlock:
		if typed != nil {
			return len(typed.Text)
		}
	case llm.ToolUseBlock:
		return len(typed.Name) + len(typed.Input)
	case *llm.ToolUseBlock:
		if typed != nil {
			return len(typed.Name) + len(typed.Input)
		}
	case llm.ToolResultBlock:
		return len(typed.Content)
	case *llm.ToolResultBlock:
		if typed != nil {
			return len(typed.Content)
		}
	case llm.ReasoningBlock:
		return len(typed.Content)
	case *llm.ReasoningBlock:
		if typed != nil {
			return len(typed.Content)
		}
	case llm.ImageBlock:
		return len(typed.Data)
	case *llm.ImageBlock:
		if typed != nil {
			return len(typed.Data)
		}
	}
	return 0
}

func compactionContextErr(ctx context.Context, result *CompactionResult) error {
	if err := ctx.Err(); err != nil {
		result.Errors = append(result.Errors, err)
		return err
	}
	return nil
}

type dropOldestToolGroupsCompactor struct{}

// NewDropOldestToolGroupsCompactor returns a compactor that collapses old tool exchanges first.
func NewDropOldestToolGroupsCompactor() Compactor {
	return dropOldestToolGroupsCompactor{}
}

func (dropOldestToolGroupsCompactor) Compact(
	ctx context.Context,
	history []llm.Message,
	budget CompactionBudget,
) (CompactionResult, error) {
	result := CompactionResult{
		History:    copyMessages(history),
		Strategies: []string{"drop_oldest_tool_groups"},
		Errors:     []error{},
	}
	if err := compactionContextErr(ctx, &result); err != nil {
		return result, err
	}
	groups, err := partitionHistory(history)
	if err != nil {
		return result, err
	}
	if err := compactionContextErr(ctx, &result); err != nil {
		return result, err
	}
	historyRunes := estimateRunes(result.History)
	if err := compactionContextErr(ctx, &result); err != nil {
		return result, err
	}
	if historyRunes <= budget.MaxRunes {
		return result, nil
	}

	protected := protectedGroups(groups)
	considered := make([]bool, len(groups))
	for historyRunes > budget.MaxRunes {
		if err := compactionContextErr(ctx, &result); err != nil {
			return result, err
		}
		candidate := -1
		for index, group := range groups {
			if group.isTool && !protected[index] && !considered[index] {
				candidate = index
				break
			}
		}
		if candidate < 0 {
			break
		}

		considered[candidate] = true
		replacement := []llm.Message{collapseToolGroup(groups[candidate])}
		if err := compactionContextErr(ctx, &result); err != nil {
			return result, err
		}
		replacementRunes := estimateRunes(replacement)
		if err := compactionContextErr(ctx, &result); err != nil {
			return result, err
		}
		groupRunes := estimateRunes(groups[candidate].messages)
		if err := compactionContextErr(ctx, &result); err != nil {
			return result, err
		}
		if replacementRunes >= groupRunes {
			continue
		}
		groups[candidate] = historyGroup{messages: replacement}
		result.DroppedGroups++
		result.Changed = true
		result.History = flattenGroups(groups)
		if err := compactionContextErr(ctx, &result); err != nil {
			return result, err
		}
		historyRunes = estimateRunes(result.History)
		if err := compactionContextErr(ctx, &result); err != nil {
			return result, err
		}
	}

	if err := compactionContextErr(ctx, &result); err != nil {
		return result, err
	}
	if historyRunes > budget.MaxRunes {
		return result, fmt.Errorf("drop oldest tool groups: %w", ErrCompactionBudgetExceeded)
	}
	return result, nil
}

func collapseToolGroup(group historyGroup) llm.Message {
	results := map[string]string{}
	for _, message := range group.messages[1:] {
		for _, block := range message.Content {
			switch typed := block.(type) {
			case llm.ToolResultBlock:
				results[typed.ToolUseID] = firstLine(typed.Content)
			case *llm.ToolResultBlock:
				if typed != nil {
					results[typed.ToolUseID] = firstLine(typed.Content)
				}
			}
		}
	}

	lines := []string{}
	for _, block := range group.messages[0].Content {
		switch typed := block.(type) {
		case llm.ToolUseBlock:
			lines = append(lines, typed.Name+": "+results[typed.ID])
		case *llm.ToolUseBlock:
			if typed != nil {
				lines = append(lines, typed.Name+": "+results[typed.ID])
			}
		}
	}
	text := toolCollapsePrefix + strings.Join(lines, "\n")
	if utf8.RuneCountInString(text) > maxToolCollapseRunes {
		runes := []rune(text)
		limit := maxToolCollapseRunes - utf8.RuneCountInString(toolCollapseSuffix)
		text = string(runes[:limit]) + toolCollapseSuffix
	}
	return llm.AssistantMessage(text)
}

func firstLine(value string) string {
	line, _, _ := strings.Cut(value, "\n")
	return strings.TrimSuffix(line, "\r")
}

type slidingWindowCompactor struct{}

// NewSlidingWindowCompactor returns a compactor that removes complete oldest groups.
func NewSlidingWindowCompactor() Compactor {
	return slidingWindowCompactor{}
}

func (slidingWindowCompactor) Compact(
	ctx context.Context,
	history []llm.Message,
	budget CompactionBudget,
) (CompactionResult, error) {
	result := CompactionResult{
		History:    copyMessages(history),
		Strategies: []string{"sliding_window"},
		Errors:     []error{},
	}
	if err := compactionContextErr(ctx, &result); err != nil {
		return result, err
	}
	groups, err := partitionHistory(history)
	if err != nil {
		return result, err
	}
	if err := compactionContextErr(ctx, &result); err != nil {
		return result, err
	}
	historyRunes := estimateRunes(result.History)
	if err := compactionContextErr(ctx, &result); err != nil {
		return result, err
	}
	if historyRunes <= budget.MaxRunes {
		return result, nil
	}

	protected := protectedGroups(groups)
	active := make([]bool, len(groups))
	for index := range active {
		active[index] = true
	}
	for historyRunes > budget.MaxRunes {
		if err := compactionContextErr(ctx, &result); err != nil {
			return result, err
		}
		candidate := -1
		for index := range groups {
			if active[index] && !protected[index] {
				candidate = index
				break
			}
		}
		if candidate < 0 {
			break
		}
		active[candidate] = false
		result.DroppedGroups++
		result.Changed = true
		result.History = flattenActiveGroups(groups, active)
		if err := compactionContextErr(ctx, &result); err != nil {
			return result, err
		}
		historyRunes = estimateRunes(result.History)
		if err := compactionContextErr(ctx, &result); err != nil {
			return result, err
		}
	}

	if err := compactionContextErr(ctx, &result); err != nil {
		return result, err
	}
	if historyRunes > budget.MaxRunes {
		return result, fmt.Errorf("sliding window: %w", ErrCompactionBudgetExceeded)
	}
	return result, nil
}

type summarizationCompactor struct {
	provider llm.Provider
}

// NewSummarizationCompactor returns a compactor that summarizes prior conversation rounds.
func NewSummarizationCompactor(provider llm.Provider) Compactor {
	return summarizationCompactor{provider: provider}
}

func (s summarizationCompactor) Compact(
	ctx context.Context,
	history []llm.Message,
	budget CompactionBudget,
) (CompactionResult, error) {
	result := CompactionResult{
		History:    copyMessages(history),
		Strategies: []string{"summarization"},
		Errors:     []error{},
	}
	groups, err := partitionHistory(history)
	if err != nil {
		return result, err
	}
	if estimateRunes(result.History) <= budget.MaxRunes {
		return result, nil
	}

	firstUser, latestUser := userGroupBounds(groups)
	if firstUser < 0 || latestUser <= firstUser+1 {
		return result, nil
	}
	span := flattenGroups(groups[firstUser+1 : latestUser])
	if len(span) == 0 {
		return result, nil
	}
	if s.provider == nil {
		return result, errors.New("agent: summarization provider is nil")
	}

	request := systemPrefix(groups)
	request = append(request, llm.UserMessage(defaultSummarizationPrompt+"\n\n"+summarizationTranscript(span)))
	response, _, err := s.provider.Chat(ctx, request, nil)
	if err != nil {
		return result, err
	}
	if response == nil {
		return result, errors.New("agent: summarization provider returned nil response")
	}
	summary := joinedText(*response)
	if strings.TrimSpace(summary) == "" {
		return result, errors.New("agent: summarization provider returned empty text")
	}

	compactedGroups := make([]historyGroup, 0, len(groups)-(latestUser-firstUser-1)+1)
	compactedGroups = append(compactedGroups, groups[:firstUser+1]...)
	compactedGroups = append(compactedGroups, historyGroup{
		messages: []llm.Message{llm.AssistantMessage("[Summary]\n" + summary)},
	})
	compactedGroups = append(compactedGroups, groups[latestUser:]...)
	result.History = flattenGroups(compactedGroups)
	result.DroppedGroups = latestUser - firstUser - 1
	result.Changed = true
	if estimateRunes(result.History) > budget.MaxRunes {
		return result, fmt.Errorf("summarization: %w", ErrCompactionBudgetExceeded)
	}
	return result, nil
}

func summarizationTranscript(history []llm.Message) string {
	lines := make([]string, 0, len(history))
	for _, message := range history {
		parts := []string{}
		for _, block := range message.Content {
			switch typed := block.(type) {
			case llm.TextBlock:
				parts = append(parts, typed.Text)
			case *llm.TextBlock:
				if typed != nil {
					parts = append(parts, typed.Text)
				}
			case llm.ToolUseBlock:
				parts = append(parts, typed.Name+": "+string(typed.Input))
			case *llm.ToolUseBlock:
				if typed != nil {
					parts = append(parts, typed.Name+": "+string(typed.Input))
				}
			case llm.ToolResultBlock:
				parts = append(parts, typed.Content)
			case *llm.ToolResultBlock:
				if typed != nil {
					parts = append(parts, typed.Content)
				}
			}
		}
		lines = append(lines, string(message.Role)+": "+strings.Join(parts, "\n"))
	}
	return strings.Join(lines, "\n")
}

func joinedText(message llm.Message) string {
	texts := []string{}
	for _, block := range message.Content {
		switch typed := block.(type) {
		case llm.TextBlock:
			texts = append(texts, typed.Text)
		case *llm.TextBlock:
			if typed != nil {
				texts = append(texts, typed.Text)
			}
		}
	}
	return strings.Join(texts, "\n")
}

type compactorChain struct {
	strategies []Compactor
}

// NewCompactorChain returns a compactor that applies each strategy at most once in order.
func NewCompactorChain(strategies ...Compactor) Compactor {
	return compactorChain{strategies: append([]Compactor{}, strategies...)}
}

// NewStandardCompactor returns the standard tool-collapse and sliding-window chain.
func NewStandardCompactor() Compactor {
	return NewCompactorChain(
		NewDropOldestToolGroupsCompactor(),
		NewSlidingWindowCompactor(),
	)
}

func (c compactorChain) Compact(
	ctx context.Context,
	history []llm.Message,
	budget CompactionBudget,
) (CompactionResult, error) {
	result := CompactionResult{
		History:    copyMessages(history),
		Strategies: []string{},
		Errors:     []error{},
	}
	if estimateRunes(result.History) <= budget.MaxRunes {
		return result, nil
	}

	for _, strategy := range c.strategies {
		candidate, err := strategy.Compact(ctx, result.History, budget)
		result.Strategies = append(result.Strategies, candidate.Strategies...)
		if err != nil {
			result.Errors = append(result.Errors, err)
			if !errors.Is(err, ErrCompactionBudgetExceeded) {
				continue
			}
		}

		if estimateRunes(candidate.History) <= estimateRunes(result.History) {
			result.History = copyMessages(candidate.History)
			result.DroppedGroups += candidate.DroppedGroups
			result.Changed = result.Changed || candidate.Changed
		}
		if estimateRunes(result.History) <= budget.MaxRunes {
			break
		}
	}
	return result, nil
}

func protectedGroups(groups []historyGroup) []bool {
	protected := make([]bool, len(groups))
	if len(groups) == 0 {
		return protected
	}
	if groups[0].isSystem {
		protected[0] = true
	}
	firstUser, latestUser := userGroupBounds(groups)
	if firstUser >= 0 {
		protected[firstUser] = true
		protected[latestUser] = true
	}
	protected[len(groups)-1] = true
	return protected
}

func userGroupBounds(groups []historyGroup) (int, int) {
	first := -1
	latest := -1
	for index, group := range groups {
		if len(group.messages) != 1 || group.messages[0].Role != llm.RoleUser {
			continue
		}
		if first < 0 {
			first = index
		}
		latest = index
	}
	return first, latest
}

func systemPrefix(groups []historyGroup) []llm.Message {
	if len(groups) == 0 || !groups[0].isSystem {
		return []llm.Message{}
	}
	return copyMessages(groups[0].messages)
}

func flattenGroups(groups []historyGroup) []llm.Message {
	total := 0
	for _, group := range groups {
		total += len(group.messages)
	}
	messages := make([]llm.Message, 0, total)
	for _, group := range groups {
		messages = append(messages, group.messages...)
	}
	return messages
}

func flattenActiveGroups(groups []historyGroup, active []bool) []llm.Message {
	messages := []llm.Message{}
	for index, group := range groups {
		if active[index] {
			messages = append(messages, group.messages...)
		}
	}
	return messages
}
