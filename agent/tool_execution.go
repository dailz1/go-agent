package agent

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

type toolSlotState uint8

const (
	toolSlotPending toolSlotState = iota
	toolSlotResolved
	toolSlotHardError
	toolSlotMissing
)

type toolSlot struct {
	state  toolSlotState
	result *tool.ToolResult
	err    error
	tool   tool.Tool
}

type toolRound struct {
	ctx, commitCtx          context.Context
	iteration               int
	calls                   []llm.ToolUseBlock
	slots                   []toolSlot
	history                 *[]llm.Message
	sess                    *persistence
	yield                   func(AgentEvent, error) bool
	materialized            int
	hardIndex, missingIndex int
	window                  int
}

func newToolRound(ctx, commitCtx context.Context, iteration int, calls []llm.ToolUseBlock, history *[]llm.Message, sess *persistence, yield func(AgentEvent, error) bool, window int) *toolRound {
	return &toolRound{
		ctx: ctx, commitCtx: commitCtx, iteration: iteration, calls: calls,
		slots: make([]toolSlot, len(calls)), history: history, sess: sess, yield: yield,
		hardIndex: -1, missingIndex: -1, window: window,
	}
}

func (r *toolRound) observe(index int) {
	switch r.slots[index].state {
	case toolSlotHardError:
		if r.hardIndex == -1 || index < r.hardIndex {
			r.hardIndex = index
		}
	case toolSlotMissing:
		if r.missingIndex == -1 || index < r.missingIndex {
			r.missingIndex = index
		}
	}
}

func (r *toolRound) markPendingMissing() {
	for index := range r.slots {
		if r.slots[index].state == toolSlotPending {
			r.slots[index].state = toolSlotMissing
			r.observe(index)
		}
	}
}

func (r *toolRound) observeAll() {
	for index := range r.slots {
		r.observe(index)
	}
}

// executeToolRound keeps the default lazy serial path separate from the
// opt-in planner while both paths share ordered settlement.
func (a *Agent) executeToolRound(ctx context.Context, iteration int, calls []llm.ToolUseBlock, history *[]llm.Message, sess *persistence, yield func(AgentEvent, error) bool, w int) bool {
	if a.toolConcurrency == 1 {
		return a.executeToolRoundSerial(ctx, iteration, calls, history, sess, yield, w)
	}
	return a.executeToolRoundParallel(ctx, iteration, calls, history, sess, yield, w)
}

func (a *Agent) executeToolRoundSerial(ctx context.Context, iteration int, calls []llm.ToolUseBlock, history *[]llm.Message, sess *persistence, yield func(AgentEvent, error) bool, w int) bool {
	round := newToolRound(ctx, ctx, iteration, calls, history, sess, yield, w)
	for index, call := range calls {
		if ctx.Err() != nil {
			round.slots[index].state = toolSlotMissing
			round.observe(index)
			return round.settle(index + 1)
		}
		result, err := a.executeTool(ctx, call)
		if err != nil {
			round.slots[index] = toolSlot{state: toolSlotHardError, err: err}
		} else {
			round.slots[index] = toolSlot{state: toolSlotResolved, result: a.applyToolResultLimit(call, result, round.window)}
		}
		round.observe(index)
		if !round.settle(index + 1) {
			return false
		}
	}
	return true
}

// settle is the sole history/event/commit path. Terminal errors always stop
// the iterator even when the consumer accepts their delivery.
func (r *toolRound) settle(frontier int) bool {
	boundary := frontier
	if r.hardIndex >= 0 && r.hardIndex < frontier {
		boundary = r.hardIndex
	} else if r.missingIndex >= 0 && r.missingIndex < frontier {
		boundary = r.missingIndex
	}
	for r.materialized < boundary && r.slots[r.materialized].state == toolSlotResolved {
		index := r.materialized
		call := r.calls[index]
		result := r.slots[index].result
		msg := llm.ToolResultMessage(call.ID, result)
		// The delivered event always carries its own copy, on durable and
		// non-durable runs alike (issue #11): a non-durable run has no write
		// boundary below, so cloning here is what keeps a consumer mutating
		// the event from reaching the tool's original result.
		eventResult := cloneToolResult(result)
		if r.sess != nil {
			// Freeze nested mutable bytes at the write boundary: the durable
			// record, history, and the round-end batch all carry this isolated
			// copy, so a consumer mutating the delivered event — down to
			// editing an image Data byte in place — can never reach them.
			msg = deepCopyMessages([]llm.Message{msg})[0]
			// json.Marshal coerces invalid UTF-8 to U+FFFD, so normalize the
			// model-visible strings here: the delivered event and the durable
			// record must stay equal across a replay.
			normalizeResultStrings(msg, eventResult)
			if err := r.sess.recordRoundResult(r.commitCtx, r.iteration, index, msg); err != nil {
				r.yield(nil, err)
				return false
			}
		}
		*r.history = append(*r.history, msg)
		r.materialized++
		if !r.yield(ToolResultEvent{ID: call.ID, Name: call.Name, Result: eventResult}, nil) {
			return false
		}
	}
	if r.hardIndex >= 0 && r.hardIndex < frontier {
		r.yield(nil, fmt.Errorf("iteration %d: tool %q: %w", r.iteration, r.calls[r.hardIndex].Name, r.slots[r.hardIndex].err))
		return false
	}
	if r.missingIndex >= 0 && r.missingIndex < frontier {
		r.yield(nil, r.ctx.Err())
		return false
	}
	if frontier != len(r.calls) {
		return true
	}
	if r.sess != nil {
		// The batch is taken from the already-isolated history snapshot, not
		// rebuilt from the live slots: it must match what was recorded and
		// delivered even if a consumer mutated the original tool result.
		results := trailingToolResults(*r.history)
		if err := r.sess.commitRound(r.commitCtx, r.iteration, results); err != nil {
			r.yield(nil, err)
			return false
		}
	}
	return true
}

// cloneToolResult copies a ToolResult deeply enough that a consumer mutating
// the delivered event — including in-place edits of image Data bytes — can
// never reach the tool's original result, the recorded round_result, or the
// round-end batch. nil/empty slice shapes are preserved.
func cloneToolResult(r *tool.ToolResult) *tool.ToolResult {
	out := &tool.ToolResult{Content: r.Content, Status: r.Status}
	if r.Data != nil {
		out.Data = bytes.Clone(r.Data)
	}
	if r.Images != nil {
		out.Images = make([]tool.Image, len(r.Images))
		for i, img := range r.Images {
			out.Images[i] = tool.Image{URL: img.URL, MIMEType: img.MIMEType}
			if img.Data != nil {
				out.Images[i].Data = bytes.Clone(img.Data)
			}
		}
	}
	return out
}

// normalizeResultStrings makes the model-visible strings valid UTF-8 in
// both the persisted message and the delivered event: json.Marshal would
// otherwise coerce invalid bytes only in the record, splitting what the
// consumer saw from what recovery replays.
func normalizeResultStrings(msg llm.Message, event *tool.ToolResult) {
	const replacement = "\uFFFD"
	for i, block := range msg.Content {
		rb, ok := block.(llm.ToolResultBlock)
		if !ok {
			continue
		}
		rb.Content = strings.ToValidUTF8(rb.Content, replacement)
		for j, img := range rb.Images {
			img.URL = strings.ToValidUTF8(img.URL, replacement)
			img.MIMEType = strings.ToValidUTF8(img.MIMEType, replacement)
			rb.Images[j] = img
		}
		msg.Content[i] = rb
	}
	event.Content = strings.ToValidUTF8(event.Content, replacement)
	for j, img := range event.Images {
		img.URL = strings.ToValidUTF8(img.URL, replacement)
		img.MIMEType = strings.ToValidUTF8(img.MIMEType, replacement)
		event.Images[j] = img
	}
}
