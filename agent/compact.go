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
	// RecordedContextCancellation reports whether Errors already records
	// the CALLER's context cancellation (as opposed to a strategy's private
	// same-sentinel diagnostic, e.g. its own timeout wrapper). Set by this
	// package's own boundary checks; nested chains read it to deduplicate
	// the caller's cancellation exactly once. Zero value is the safe
	// default: unrecorded strategies never suppress the caller's record.
	// Known limitation: the marker identifies a cancellation of the context
	// this Compact invocation received. A wrapper that runs an inner chain
	// on a private child context and forwards the inner result carries the
	// child's marker; the outer caller's cancellation diagnostic may then
	// be suppressed (or duplicated for %w-wrapped forwarding) — a
	// log-cosmetic residual on contrived compositions; the returned error
	// contract is unaffected.
	RecordedContextCancellation bool
	// ReturnedError is the error this result is being returned with, when
	// the producer declares it (chain machinery sets it; zero means the
	// producer makes no declaration). Consumers use it to distinguish "the
	// candidate already recorded its own returned error" from "a private
	// same-sentinel diagnostic that merely matches the returned error".
	ReturnedError error
}

// Compactor reduces message history while preserving valid message groups.
// provider and opts carry the calling round's effective provider and request
// options: AI-summarizing strategies must follow them (a thread that hot
// switched providers summarizes on the switched-to provider with the
// thread's effective model), while neutral strategies ignore both. The
// in-loop caller resolves both from the thread's config projection; manual
// callers pass what they want used.
//
// Input isolation: the in-loop caller passes a deep copy of the live
// history, so implementations may mutate or retain the input freely;
// result.History must be self-owned memory (the kernel deep-copies it on
// adoption) and must not rewrite the system prefix (validated against the
// untouched original). Histories containing block types the kernel cannot
// deep-copy skip compaction; results containing them are rejected (both
// non-fatal, Warn and continue).
//
// Chain adoption semantics (issue #13): only candidates with Changed=true
// replace the working history — Changed=false is a no-op declaration and is
// never adopted, even when the returned history is smaller. A Changed=true
// candidate must be a structurally valid, non-empty history that preserves
// the system prefix and contains only deep-copyable block types; invalid
// candidates are recorded as strategy errors and the chain continues with
// the next stage, and a Changed=true candidate with empty history is
// recorded as a diagnostic. Neither case terminates the chain early. An
// accepted candidate is deep-copied at adoption, so the stage cannot mutate
// the chain's working history through retained references.
//
// Cancellation at stage boundaries (issue #14): the real context is checked
// before each stage and again after it returns; a confirmed cancellation
// returns (result, ctx.Err()) without invoking later stages. A stage
// reporting a cancellation-class error while the context is alive belongs to
// the stage's own internal context (e.g. a private timeout wrapper) and is
// handled as an ordinary strategy error, so later stages still run. Nested
// chains' Errors are merged into the result on every stage return,
// independent of adoption; a returned error the candidate already recorded
// is not recorded twice.
type Compactor interface {
	Compact(ctx context.Context, history []llm.Message, budget CompactionBudget, provider llm.Provider, opts []llm.Option) (CompactionResult, error)
}

// CompactFunc adapts a function to Compactor.
type CompactFunc func(
	ctx context.Context,
	history []llm.Message,
	budget CompactionBudget,
	provider llm.Provider,
	opts []llm.Option,
) (CompactionResult, error)

// Compact delegates to f.
func (f CompactFunc) Compact(
	ctx context.Context,
	history []llm.Message,
	budget CompactionBudget,
	provider llm.Provider,
	opts []llm.Option,
) (CompactionResult, error) {
	return f(ctx, history, budget, provider, opts)
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

func estimateRunesMode(history []llm.Message) (int, bool) {
	encoded, err := json.Marshal(history)
	if err == nil {
		return utf8.RuneCount(encoded), true
	}
	return fallbackRunes(history), false
}

func estimateRunes(history []llm.Message) int {
	runes, _ := estimateRunesMode(history)
	return runes
}

func fallbackRunes(history []llm.Message) int {
	total := 0
	for _, message := range history {
		total += len(message.Role)
		for _, block := range message.Content {
			total += fallbackBlockSize(block)
		}
	}
	return total
}

// groupEstimate sizes one group in the mode the whole history encodes in:
// the estimator is deterministic, so if the whole history marshals, every
// group marshals too, and per-group JSON rune counts sum exactly to the
// whole (see dropDelta); otherwise every group uses the additive
// fallback byte sum, which also sums exactly.
func groupEstimate(messages []llm.Message, jsonMode bool) int {
	if jsonMode {
		return estimateRunes(messages)
	}
	return fallbackRunes(messages)
}

// jsonSizeLowerBound returns a conservative lower bound on the runes a
// group contributes to the whole-history JSON encoding: text counted in
// runes (JSON escaping only adds), inline images counted at their base64
// expansion (4 bytes per 3 raw bytes). The bound never exceeds the true
// delta, so routing on it can only mis-pick the slower exact path, never
// an incorrect one.
func jsonSizeLowerBound(messages []llm.Message) int {
	total := 0
	for _, message := range messages {
		for _, block := range message.Content {
			switch typed := block.(type) {
			case llm.TextBlock:
				total += utf8.RuneCountInString(typed.Text)
			case *llm.TextBlock:
				if typed != nil {
					total += utf8.RuneCountInString(typed.Text)
				}
			case llm.ToolUseBlock:
			case *llm.ToolUseBlock:
				// Input is json.RawMessage: MarshalJSON compacts its
				// insignificant whitespace, so raw runes can only overcount
				// the delta. Omitted from the bound on purpose.
			case llm.ToolResultBlock:
				total += utf8.RuneCountInString(typed.Content)
				total += imageBase64Runes(typed.Images)
			case *llm.ToolResultBlock:
				if typed != nil {
					total += utf8.RuneCountInString(typed.Content)
					total += imageBase64Runes(typed.Images)
				}
			case llm.ReasoningBlock:
				total += utf8.RuneCountInString(typed.Content)
			case *llm.ReasoningBlock:
				if typed != nil {
					total += utf8.RuneCountInString(typed.Content)
				}
			case llm.ReasoningItemBlock:
				for _, summary := range typed.Summary {
					total += utf8.RuneCountInString(summary)
				}
				total += utf8.RuneCountInString(typed.EncryptedContent)
			case *llm.ReasoningItemBlock:
				if typed != nil {
					for _, summary := range typed.Summary {
						total += utf8.RuneCountInString(summary)
					}
					total += utf8.RuneCountInString(typed.EncryptedContent)
				}
			case llm.ImageBlock:
				total += len(typed.Data) * 4 / 3
			case *llm.ImageBlock:
				if typed != nil {
					total += len(typed.Data) * 4 / 3
				}
			}
		}
	}
	return total
}

func imageBase64Runes(images []llm.ImageBlock) int {
	n := 0
	for _, img := range images {
		n += len(img.Data) * 4 / 3
	}
	return n
}

// knownBlockTypes reports whether every block is one of the kernel's known
// types. Known types are plain structs with deterministic JSON encodings —
// the incremental accounting relies on a group encoding identically inside
// the whole history and alone. An embedding wrapper (e.g. a struct
// embedding llm.TextBlock) satisfies the sealed interface and the whole
// history can still marshal, but its custom MarshalJSON may encode
// differently across calls, so such histories must keep the full-recount
// path.
func knownBlockTypes(history []llm.Message) bool {
	for i := range history {
		for _, block := range history[i].Content {
			if !clonableBlock(block) {
				return false
			}
		}
	}
	return true
}

// dropDelta returns how much the whole-history estimate shrinks when one
// group of size runes is removed. A group's own encoding carries its "[]"
// brackets, but the whole-history encoding collapses them into the single
// outer pair — so only the group's inner runes plus one comma disappear:
// JSON mode delta is size-1. The fallback byte sum is purely additive.
// Sliding-window removals always leave the protected final group in place,
// so the arithmetic never reaches the empty history.
func dropDelta(size int, jsonMode bool) int {
	if jsonMode {
		return size - 1
	}
	return size
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
		n := len(typed.Content)
		for _, img := range typed.Images {
			n += len(img.Data)
		}
		return n
	case *llm.ToolResultBlock:
		if typed != nil {
			n := len(typed.Content)
			for _, img := range typed.Images {
				n += len(img.Data)
			}
			return n
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
		result.RecordedContextCancellation = true
		return err
	}
	return nil
}

// sameErrorObject reports whether a and b are the same error object. The
// interface comparison can panic for identical non-comparable dynamic types
// (e.g. a named slice implementing error); such errors cannot be tested for
// identity, and are treated as distinct — both diagnostics are kept rather
// than dropped on a comparison technicality.
func sameErrorObject(a, b error) (same bool) {
	defer func() {
		if recover() != nil {
			same = false
		}
	}()
	return a == b
}

// containsIdentical reports whether errs record the exact same error object
// as target (identity, not errors.Is: a joined or wrapped error can
// partially match a sentinel while carrying an independent cause). Panic
// for non-comparable dynamic types is contained by sameErrorObject.
func containsIdentical(errs []error, target error) bool {
	for _, recorded := range errs {
		if sameErrorObject(recorded, target) {
			return true
		}
	}
	return false
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
	_ llm.Provider, // neutral strategy: provider and options carry no meaning
	_ []llm.Option,
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
	knownTypes := knownBlockTypes(result.History)
	if err := compactionContextErr(ctx, &result); err != nil {
		return result, err
	}
	historyRunes, jsonMode := estimateRunesMode(result.History)
	if !knownTypes {
		// Custom MarshalJSON on embedding-wrapper blocks breaks the
		// elementwise exactness the incremental accounting relies on, and
		// marshalers run during estimateRunesMode — the gate must inspect
		// the blocks before any custom code can run.
		jsonMode = false
	}
	if err := compactionContextErr(ctx, &result); err != nil {
		return result, err
	}
	if historyRunes <= budget.MaxRunes {
		return result, nil
	}

	protected := protectedGroups(groups)
	considered := make([]bool, len(groups))
	materialized := !jsonMode
	if jsonMode {
		// Incremental exact accounting: replacing one group with its
		// collapse keeps the group count unchanged, so the whole estimate
		// moves by replacementRunes - groupRunes. Candidates are sized on
		// demand; groups never collapsed are never encoded.
		for historyRunes > budget.MaxRunes {
			if err := compactionContextErr(ctx, &result); err != nil {
				if result.Changed {
					result.History = flattenGroups(groups)
				}
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
				if result.Changed {
					result.History = flattenGroups(groups)
				}
				return result, err
			}
			groupRunes := groupEstimate(groups[candidate].messages, true)
			if err := compactionContextErr(ctx, &result); err != nil {
				if result.Changed {
					result.History = flattenGroups(groups)
				}
				return result, err
			}
			replacementRunes := groupEstimate(replacement, true)
			if err := compactionContextErr(ctx, &result); err != nil {
				if result.Changed {
					result.History = flattenGroups(groups)
				}
				return result, err
			}
			if replacementRunes >= groupRunes {
				continue
			}
			groups[candidate] = historyGroup{messages: replacement}
			result.DroppedGroups++
			result.Changed = true
			historyRunes += replacementRunes - groupRunes
		}
	} else {
		// Marshal failed at entry (e.g. malformed streamed tool args):
		// keep the original per-iteration estimation, so the accounting
		// stays exact if a collapse drops the failing group and encoding
		// starts succeeding mid-loop.
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
	}

	if result.Changed && !materialized {
		result.History = flattenGroups(groups)
		if err := compactionContextErr(ctx, &result); err != nil {
			return result, err
		}
		historyRunes = estimateRunes(result.History)
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
	_ llm.Provider, // neutral strategy: provider and options carry no meaning
	_ []llm.Option,
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
	knownTypes := knownBlockTypes(result.History)
	if err := compactionContextErr(ctx, &result); err != nil {
		return result, err
	}
	historyRunes, jsonMode := estimateRunesMode(result.History)
	if !knownTypes {
		// Custom MarshalJSON on embedding-wrapper blocks breaks the
		// elementwise exactness the incremental accounting relies on, and
		// marshalers run during estimateRunesMode — the gate must inspect
		// the blocks before any custom code can run.
		jsonMode = false
	}
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
	materialized := !jsonMode
	if jsonMode {
		// Incremental exact accounting: the JSON encoding is elementwise
		// plus one comma between adjacent groups, so removing a group of s
		// runes lowers the whole estimate by s-1 (the group's own brackets
		// collapse into the single outer pair). Only groups that are not
		// dropped stay unencoded; the flattened history and one full
		// verification encode run once after the loop.
		measured := false
		for historyRunes > budget.MaxRunes {
			if err := compactionContextErr(ctx, &result); err != nil {
				if result.Changed && !materialized {
					result.History = flattenActiveGroups(groups, active)
				}
				return result, err
			}
			candidate := -1
			more := false
			for index := range groups {
				if active[index] && !protected[index] {
					if candidate < 0 {
						candidate = index
						continue
					}
					more = true
					break
				}
			}
			if candidate < 0 {
				break
			}
			active[candidate] = false
			result.DroppedGroups++
			result.Changed = true
			if measured && more {
				// Cancel between the scan and the sizing encode: the
				// committed removals must be materialized before the
				// discarded group is serialized.
				if err := compactionContextErr(ctx, &result); err != nil {
					result.History = flattenActiveGroups(groups, active)
					return result, err
				}
				bound := jsonSizeLowerBound(groups[candidate].messages)
				overflow := historyRunes - budget.MaxRunes
				// Direct retained-history measurement when the discarded
				// group provably covers the remaining overflow (lower bound
				// >= overflow means the drop meets the budget), or when it
				// dominates the history (each such recount at least halves
				// the remaining history). Otherwise serialize the discarded
				// group once for the exact subtraction.
				if bound >= overflow || 2*bound >= historyRunes {
					result.History = flattenActiveGroups(groups, active)
					if err := compactionContextErr(ctx, &result); err != nil {
						return result, err
					}
					historyRunes = estimateRunes(result.History)
					materialized = true
					measured = true
					continue
				}
				// Cancel again after the bound scan: it walks large text
				// payloads, and the sizing encode below serializes the whole
				// discarded group.
				if err := compactionContextErr(ctx, &result); err != nil {
					result.History = flattenActiveGroups(groups, active)
					return result, err
				}
				historyRunes -= dropDelta(groupEstimate(groups[candidate].messages, true), true)
				materialized = false
				continue
			}
			// First removal or last eligible group: measure the retained
			// history directly instead of sizing the discarded group,
			// which may be a large image payload being dropped.
			result.History = flattenActiveGroups(groups, active)
			if err := compactionContextErr(ctx, &result); err != nil {
				return result, err
			}
			historyRunes = estimateRunes(result.History)
			materialized = true
			measured = true
			if !more {
				break
			}
		}
	} else {
		// Marshal failed at entry (e.g. malformed streamed tool args):
		// keep the original per-iteration estimation, so the accounting
		// stays exact if a removal drops the failing group and encoding
		// starts succeeding mid-loop.
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
	}

	if result.Changed && !materialized {
		result.History = flattenActiveGroups(groups, active)
		if err := compactionContextErr(ctx, &result); err != nil {
			return result, err
		}
		historyRunes = estimateRunes(result.History)
	}
	if err := compactionContextErr(ctx, &result); err != nil {
		return result, err
	}
	if historyRunes > budget.MaxRunes {
		return result, fmt.Errorf("sliding window: %w", ErrCompactionBudgetExceeded)
	}
	return result, nil
}

type summarizationCompactor struct{}

// NewSummarizationCompactor returns a compactor that summarizes prior
// conversation rounds. The provider and options of the summarization call
// come from each Compact invocation, so the summary follows the thread's
// effective provider (including cross-provider hot switches) and effective
// model; a nil provider fails with the existing typed error and degrades
// through the usual non-fatal compaction path.
func NewSummarizationCompactor() Compactor {
	return summarizationCompactor{}
}

func (summarizationCompactor) Compact(
	ctx context.Context,
	history []llm.Message,
	budget CompactionBudget,
	provider llm.Provider,
	opts []llm.Option,
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
	if provider == nil {
		return result, errors.New("agent: summarization provider is nil")
	}

	request := systemPrefix(groups)
	request = append(request, llm.UserMessage(defaultSummarizationPrompt+"\n\n"+summarizationTranscript(span)))
	// Clone first, then check, then dispatch (issue #14): the request copy
	// can be sizable, so a context canceled during it must not reach
	// provider.Chat — the check sits directly ahead of the call on the
	// already-prepared request.
	clonedRequest := deepCopyMessages(request)
	if err := compactionContextErr(ctx, &result); err != nil {
		return result, err
	}
	response, _, err := provider.Chat(ctx, clonedRequest, nil, opts...)
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

// compactionAdoptionHook, when non-nil, runs at each adoption-gate entry.
// Test seam (same pattern as sleepFor/timeNow): it marks the point where
// adoption work starts, so tests can cancel the context inside the gate.
var compactionAdoptionHook func()

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
	provider llm.Provider,
	opts []llm.Option,
) (CompactionResult, error) {
	result := CompactionResult{
		History:    copyMessages(history),
		Strategies: []string{},
		Errors:     []error{},
	}
	if estimateRunes(result.History) <= budget.MaxRunes {
		// Within-budget early return still reports cancellation (issue #14):
		// a canceled context with a fitting history must not read as a
		// successful no-op compaction, matching the drop-oldest and
		// sliding-window compactors' boundary checks.
		if err := compactionContextErr(ctx, &result); err != nil {
			return result, err
		}
		return result, nil
	}

	for i, strategy := range c.strategies {
		// Cancellation at the stage boundary (issue #14): the real context
		// is checked before dispatching so later stages never run with a
		// dead context. The early check also skips the stage-input copy on
		// an already-dead context; cloning large histories is not free and
		// the context can lapse during it, so it is rechecked on the
		// prepared input below.
		if err := compactionContextErr(ctx, &result); err != nil {
			return result, err
		}
		// Stages are Compactor implementations too: isolate each stage's
		// input so a failed stage's in-place mutations cannot leak into
		// later stages or the adopted result (issue #9 contract).
		stageInput := deepCopyMessages(result.History)
		if err := compactionContextErr(ctx, &result); err != nil {
			return result, err
		}
		candidate, err := strategy.Compact(ctx, stageInput, budget, provider, opts)
		result.Strategies = append(result.Strategies, candidate.Strategies...)
		// Nested-chain diagnostics merge on every stage return, independent
		// of adoption and of the stage's own error (issue #14).
		result.Errors = append(result.Errors, candidate.Errors...)
		if candidate.RecordedContextCancellation {
			result.RecordedContextCancellation = true
		}
		// The caller's context state outranks whatever the stage reported:
		// a private DeadlineExceeded from the stage's own timeout wrapper
		// must not masquerade as the caller's cancellation, and a caller
		// cancellation during the stage stops the chain even when the stage
		// returned an ordinary error or success. The stage's own returned
		// failure is still recorded unless it IS the caller's cancellation
		// object (identity via sameErrorObject, not errors.Is: a join or a
		// wrapped private timeout matches the sentinel partially while
		// carrying an independent cause; non-comparable error types are
		// treated as distinct). The caller's cancellation record is
		// suppressed only when the stage explicitly recorded it for the
		// caller's context (RecordedContextCancellation — a nested chain
		// that recorded ctx.Err() and returned it) and returned that same
		// object; a stage's same-sentinel private diagnostic is a distinct
		// event and never stands in for the caller's own cancellation.
		if cerr := ctx.Err(); cerr != nil {
			if err != nil && !sameErrorObject(err, cerr) && !containsIdentical(candidate.Errors, err) {
				result.Errors = append(result.Errors, err)
			}
			if !(candidate.RecordedContextCancellation && sameErrorObject(err, cerr)) {
				result.Errors = append(result.Errors, cerr)
			}
			result.RecordedContextCancellation = true
			result.ReturnedError = cerr
			return result, cerr
		}
		if err != nil {
			// A cancellation-class error reported while the real context is
			// alive belongs to the stage's own internal context (e.g. a
			// private timeout wrapper): treat it as an ordinary stage error
			// so later stages still run. Record it unless the stage declared
			// this exact error as its returned error (identity, not
			// errors.Is: two private attempts sharing Go's singleton sentinel
			// are distinct events and both stay recorded).
			if !(candidate.ReturnedError != nil && sameErrorObject(candidate.ReturnedError, err)) {
				result.Errors = append(result.Errors, err)
			}
			if !errors.Is(err, ErrCompactionBudgetExceeded) {
				continue
			}
		}

		// Adoption gate (issue #13): validate before adopting and before
		// the budget early-exit, so an invalid small candidate can neither
		// poison later stages nor stop the chain.
		if compactionAdoptionHook != nil {
			compactionAdoptionHook()
		}
		if !candidate.Changed {
			continue
		}
		if len(candidate.History) == 0 {
			result.Errors = append(result.Errors, fmt.Errorf(
				"compaction chain stage %d returned Changed=true with empty history", i+1))
			continue
		}
		if hasUnsupportedBlocks(candidate.History) {
			result.Errors = append(result.Errors, fmt.Errorf(
				"compaction chain stage %d returned blocks outside the clone whitelist", i+1))
			continue
		}
		if err := validateCompacted(result.History, candidate.History); err != nil {
			result.Errors = append(result.Errors, fmt.Errorf(
				"compaction chain stage %d returned an invalid candidate: %w", i+1, err))
			continue
		}
		if estimateRunes(candidate.History) <= estimateRunes(result.History) {
			result.History = deepCopyMessages(candidate.History)
			result.DroppedGroups += candidate.DroppedGroups
			result.Changed = true
		}
		if estimateRunes(result.History) <= budget.MaxRunes {
			break
		}
	}
	// Final recheck (issue #14): adoption work (validation, size
	// estimation, deep copy) runs after the last boundary check, so the
	// context can lapse inside the gate — cancellation during adoption
	// must take the cancellation path, not a successful return.
	if err := compactionContextErr(ctx, &result); err != nil {
		result.ReturnedError = err
		return result, err
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
