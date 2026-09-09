package agent

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/dailz1/go-agent/pkg/llm"
)

// callAccum collects the fragments of a single tool call as they stream in.
type callAccum struct {
	id   string
	name string
	args strings.Builder
}

// toolCallAccum assembles streaming Chunk fragments into complete
// ToolUseBlock and TextBlock values.
type toolCallAccum struct {
	calls            map[int]*callAccum
	textBuilder      strings.Builder
	reasoningBuilder strings.Builder
	orphanArgs       map[int]bool
	lastUsage        *llm.Usage
}

// feed processes a single streaming chunk, routing it by type.
func (a *toolCallAccum) feed(chunk llm.Chunk) {
	switch c := chunk.(type) {
	case llm.TextDeltaChunk:
		a.textBuilder.WriteString(c.Text)
	case llm.ToolCallStartChunk:
		if a.calls == nil {
			a.calls = make(map[int]*callAccum)
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
//   - a tool call is missing id or name (StartChunk was lost or overwritten)
//   - ToolCallArgsChunk arrived for an index with no matching ToolCallStartChunk
func (a *toolCallAccum) assemble() ([]llm.ToolUseBlock, error) {
	if len(a.calls) == 0 && len(a.orphanArgs) == 0 {
		return nil, nil
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
	a.lastUsage = nil
	a.textBuilder.Reset()
	a.reasoningBuilder.Reset()
}

// usage returns the last non-nil Usage captured from a DoneChunk.
func (a *toolCallAccum) usage() *llm.Usage { return a.lastUsage }
