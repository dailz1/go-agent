package agent

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/dailz1/go-agent/llm"
)

// callAccum collects the fragments of a single tool call as they stream in.
type callAccum struct {
	id   string
	name string
	args strings.Builder
}

// toolCallAccum assembles streaming Chunk fragments into complete
// ToolUseBlock and TextBlock values.
// indexedBlock pairs an output-array position with the block it produced.
type indexedBlock struct {
	index int64
	block llm.ContentBlock
}

type toolCallAccum struct {
	calls                  map[int]*callAccum
	textBuilder            strings.Builder
	reasoningBuilder       strings.Builder
	ordered                []indexedBlock
	hasReasoningItems      bool
	textGroupOpen          bool
	textIndex              int64
	orphanArgs             map[int]bool
	duplicateToolCallIndex *int
	lastUsage              *llm.Usage
}

// feed processes a single streaming chunk, routing it by type.
func (a *toolCallAccum) feed(chunk llm.Chunk) {
	switch c := chunk.(type) {
	case llm.TextDeltaChunk:
		// Indexed protocols (Responses) may interleave multiple message
		// items: close the current text group when the item index changes.
		if a.textGroupOpen && c.OutputIndex != a.textIndex {
			a.flushTextGroup()
		}
		if !a.textGroupOpen {
			a.textGroupOpen = true
			a.textIndex = c.OutputIndex
		}
		a.textBuilder.WriteString(c.Text)
	case llm.ToolCallStartChunk:
		if a.calls == nil {
			a.calls = make(map[int]*callAccum)
		}
		if _, exists := a.calls[c.Index]; exists {
			if a.duplicateToolCallIndex == nil {
				index := c.Index
				a.duplicateToolCallIndex = &index
			}
			return
		}
		a.calls[c.Index] = &callAccum{id: c.ID, name: c.Name}
	case llm.ToolCallArgsChunk:
		if acc, ok := a.calls[c.Index]; ok {
			acc.args.WriteString(c.Delta)
		} else {
			if a.orphanArgs == nil {
				a.orphanArgs = make(map[int]bool)
			}
			a.orphanArgs[c.Index] = true
		}
	case llm.ReasoningItemChunk:
		// Entry-level reasoning item at its own output position: recorded for
		// index-ordered assembly (Responses protocol).
		a.hasReasoningItems = true
		a.ordered = append(a.ordered, indexedBlock{index: c.OutputIndex, block: c.Item})
	case llm.DoneChunk:
		if c.Usage != nil {
			a.lastUsage = c.Usage
		}
	case llm.ReasoningDeltaChunk:
		a.reasoningBuilder.WriteString(c.Text)
	default:
		// ignore unknown chunk types
	}
}

// assemble builds all accumulated tool call fragments into complete
// ToolUseBlock values sorted by Index. Returns nil if no tool calls were
// accumulated.
//
// Returns an error if the stream was corrupted:
//   - a tool call is missing id or name (StartChunk was lost)
//   - a ToolCallStartChunk repeats an index already seen in this stream
//   - ToolCallArgsChunk arrived for an index with no matching ToolCallStartChunk
func (a *toolCallAccum) assemble() ([]llm.ToolUseBlock, error) {
	if len(a.calls) == 0 && len(a.orphanArgs) == 0 {
		return nil, nil
	}
	if a.duplicateToolCallIndex != nil {
		return nil, fmt.Errorf("accumulator: duplicate tool call index %d", *a.duplicateToolCallIndex)
	}

	for idx, acc := range a.calls {
		if acc.id == "" {
			return nil, fmt.Errorf(
				"stream response contained incomplete tool call at index %d: missing id (start chunk lost)",
				idx,
			)
		}
		if acc.name == "" {
			return nil, fmt.Errorf(
				"stream response contained incomplete tool call at index %d: missing name (start chunk lost)",
				idx,
			)
		}
	}

	if len(a.orphanArgs) > 0 {
		indices := a.orphanIndices()
		return nil, fmt.Errorf(
			"stream response contained orphan tool call args at index(es) %v without matching start chunks",
			indices,
		)
	}

	type indexed struct {
		idx int
		blk llm.ToolUseBlock
	}

	items := make([]indexed, 0, len(a.calls))
	for idx, acc := range a.calls {
		items = append(items, indexed{
			idx: idx,
			blk: llm.ToolUseBlock{
				Type:  "tool_use",
				ID:    acc.id,
				Name:  acc.name,
				Input: json.RawMessage(acc.args.String()),
			},
		})
	}

	slices.SortFunc(items, func(a, b indexed) int { return a.idx - b.idx })

	result := make([]llm.ToolUseBlock, len(items))
	for i, it := range items {
		result[i] = it.blk
	}
	return result, nil
}

func (a *toolCallAccum) orphanIndices() []int {
	if len(a.orphanArgs) == 0 {
		return nil
	}
	indices := make([]int, 0, len(a.orphanArgs))
	for idx := range a.orphanArgs {
		indices = append(indices, idx)
	}
	slices.Sort(indices)
	return indices
}

// textBlocks returns the accumulated text as a TextBlock slice, or nil if no
// text was accumulated.
func (a *toolCallAccum) textBlocks() []llm.ContentBlock {
	if a.textBuilder.Len() == 0 {
		return nil
	}
	return []llm.ContentBlock{
		llm.TextBlock{
			Type: "text",
			Text: a.textBuilder.String(),
		},
	}
}

// reasoningBlocks returns the accumulated reasoning as a ReasoningBlock slice,
// or nil if no reasoning was accumulated.
func (a *toolCallAccum) reasoningBlocks() []llm.ContentBlock {
	if a.reasoningBuilder.Len() == 0 {
		return nil
	}
	return []llm.ContentBlock{
		llm.ReasoningBlock{
			Type:    "reasoning",
			Content: a.reasoningBuilder.String(),
		},
	}
}

// reset clears all accumulated state.
func (a *toolCallAccum) reset() {
	a.calls = nil
	a.orphanArgs = nil
	a.duplicateToolCallIndex = nil
	a.lastUsage = nil
	a.ordered = nil
	a.hasReasoningItems = false
	a.textGroupOpen = false
	a.textIndex = 0
	a.textBuilder.Reset()
	a.reasoningBuilder.Reset()
}

// flushTextGroup closes the current indexed text group into the ordered
// sequence.
func (a *toolCallAccum) flushTextGroup() {
	if a.textBuilder.Len() > 0 {
		a.ordered = append(a.ordered, indexedBlock{
			index: a.textIndex,
			block: llm.TextBlock{Type: "text", Text: a.textBuilder.String()},
		})
	}
	a.textBuilder.Reset()
	a.textGroupOpen = false
}

// assembleContent builds the assistant message content: item-indexed merge
// (reasoning items and function calls interleaved in declaration order) when
// the protocol delivered reasoning items, or the legacy bucketed order
// (reasoning, text, tool calls) otherwise. It returns the full content plus
// the tool-call blocks.
func (a *toolCallAccum) assembleContent() ([]llm.ContentBlock, []llm.ToolUseBlock, error) {
	callBlocks, err := a.assemble()
	if err != nil {
		return nil, nil, err
	}

	// Legacy bucketed order: reasoning, then text (straight from the
	// builder), then tool calls. No item-indexed content exists here.
	if !a.hasReasoningItems {
		var content []llm.ContentBlock
		content = append(content, a.reasoningBlocks()...)
		content = append(content, a.textBlocks()...)
		for _, cb := range callBlocks {
			content = append(content, cb)
		}
		return content, callBlocks, nil
	}

	// Indexed merge: flush the open text group into the ordered sequence,
	// then interleave reasoning items, text groups, and tool calls by their
	// shared output index.
	a.flushTextGroup()
	entries := append([]indexedBlock(nil), a.ordered...)
	indexByID := make(map[string]int64, len(a.calls))
	for idx, acc := range a.calls {
		indexByID[acc.id] = int64(idx)
	}
	for _, tb := range callBlocks {
		entries = append(entries, indexedBlock{index: indexByID[tb.ID], block: tb})
	}
	slices.SortFunc(entries, func(x, y indexedBlock) int { return int(x.index - y.index) })
	content := make([]llm.ContentBlock, 0, len(entries))
	for _, e := range entries {
		content = append(content, e.block)
	}
	return content, callBlocks, nil
}

// usage returns the last non-nil Usage captured from a DoneChunk.
func (a *toolCallAccum) usage() *llm.Usage { return a.lastUsage }
