package agent

import (
	"context"
	"fmt"

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
}

func newToolRound(ctx, commitCtx context.Context, iteration int, calls []llm.ToolUseBlock, history *[]llm.Message, sess *persistence, yield func(AgentEvent, error) bool) *toolRound {
	return &toolRound{
		ctx: ctx, commitCtx: commitCtx, iteration: iteration, calls: calls,
		slots: make([]toolSlot, len(calls)), history: history, sess: sess, yield: yield,
		hardIndex: -1, missingIndex: -1,
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
func (a *Agent) executeToolRound(ctx context.Context, iteration int, calls []llm.ToolUseBlock, history *[]llm.Message, sess *persistence, yield func(AgentEvent, error) bool) bool {
	if a.toolConcurrency == 1 {
		return a.executeToolRoundSerial(ctx, iteration, calls, history, sess, yield)
	}
	return a.executeToolRoundParallel(ctx, iteration, calls, history, sess, yield)
}

func (a *Agent) executeToolRoundSerial(ctx context.Context, iteration int, calls []llm.ToolUseBlock, history *[]llm.Message, sess *persistence, yield func(AgentEvent, error) bool) bool {
	round := newToolRound(ctx, ctx, iteration, calls, history, sess, yield)
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
			round.slots[index] = toolSlot{state: toolSlotResolved, result: a.applyToolResultLimit(call, result)}
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
		*r.history = append(*r.history, llm.ToolResultMessage(call.ID, result))
		r.materialized++
		if !r.yield(ToolResultEvent{ID: call.ID, Name: call.Name, Result: result}, nil) {
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
		results := make([]llm.Message, len(r.calls))
		for index, call := range r.calls {
			results[index] = llm.ToolResultMessage(call.ID, r.slots[index].result)
		}
		if err := r.sess.commitRound(r.commitCtx, r.iteration, results); err != nil {
			r.yield(nil, err)
			return false
		}
	}
	return true
}
