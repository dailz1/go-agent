package agent

import (
	"context"
	"fmt"
	"sync"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

func (a *Agent) executeToolRoundParallel(ctx context.Context, iteration int, calls []llm.ToolUseBlock, history *[]llm.Message, sess *persistence, yield func(AgentEvent, error) bool) bool {
	round := newToolRound(ctx, context.WithoutCancel(ctx), iteration, calls, history, sess, yield)
	if !a.planParallelToolRound(ctx, calls, round.slots) {
		round.markPendingMissing()
		round.observeAll()
		return round.settle(len(calls))
	}
	workers := min(a.toolConcurrency, len(calls))
	if workers > 0 {
		a.dispatchParallelToolRound(ctx, calls, round.slots, workers)
	}
	if ctx.Err() != nil {
		round.markPendingMissing()
	}
	round.observeAll()
	return round.settle(len(calls))
}

// planParallelToolRound freezes execution handles and Info, then runs every
// approval callback in declaration order before any Execute can start.
func (a *Agent) planParallelToolRound(ctx context.Context, calls []llm.ToolUseBlock, slots []toolSlot) bool {
	for index, call := range calls {
		if ctx.Err() != nil {
			return false
		}
		t, ok := a.registry.Get(call.Name)
		if !ok {
			slots[index] = toolSlot{state: toolSlotResolved, result: a.limitedToolError(call, "tool %q not found", call.Name)}
			continue
		}
		info, err := toolInfoAtPlannerBoundary(t)
		if err != nil {
			slots[index] = toolSlot{state: toolSlotHardError, err: err}
			return false
		}
		slots[index].tool = t
		if !info.RequiresApproval {
			continue
		}
		if a.approvalFn == nil {
			slots[index] = toolSlot{state: toolSlotResolved, result: a.limitedToolError(call, "tool %q requires approval but no approval callback is configured", call.Name)}
			continue
		}
		a.logger.Warn("tool requires approval", "tool_name", call.Name, "tool_call_id", call.ID)
		approved, err := a.approveAtPlannerBoundary(info, call.Input)
		if err != nil {
			slots[index] = toolSlot{state: toolSlotHardError, err: err}
			return false
		}
		if !approved {
			a.logger.Warn("tool execution rejected", "tool_name", call.Name, "tool_call_id", call.ID)
			slots[index] = toolSlot{state: toolSlotResolved, result: a.limitedToolError(call, "tool %q execution rejected by user", call.Name)}
		}
	}
	return true
}

func toolInfoAtPlannerBoundary(t tool.Tool) (info tool.ToolInfo, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("tool info panicked: %v", recovered)
		}
	}()
	return t.Info(), nil
}

func (a *Agent) approveAtPlannerBoundary(info tool.ToolInfo, args []byte) (approved bool, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("approval callback panicked: %v", recovered)
		}
	}()
	return a.approvalFn(info, args), nil
}

func (a *Agent) dispatchParallelToolRound(ctx context.Context, calls []llm.ToolUseBlock, slots []toolSlot, workers int) {
	jobs := make(chan int)
	var workersDone sync.WaitGroup
	workersDone.Add(workers)
	for range workers {
		go func() {
			defer workersDone.Done()
			for index := range jobs {
				result, err := a.executeToolHandle(ctx, calls[index], slots[index].tool)
				if err != nil {
					slots[index] = toolSlot{state: toolSlotHardError, err: err}
					continue
				}
				slots[index] = toolSlot{state: toolSlotResolved, result: a.applyToolResultLimit(calls[index], result)}
			}
		}()
	}
	for index := range calls {
		if slots[index].state != toolSlotPending {
			continue
		}
		if ctx.Err() != nil {
			break
		}
		select {
		case jobs <- index:
		case <-ctx.Done():
			close(jobs)
			workersDone.Wait()
			return
		}
	}
	close(jobs)
	workersDone.Wait()
}

func (a *Agent) executeToolHandle(ctx context.Context, call llm.ToolUseBlock, t tool.Tool) (result *tool.ToolResult, err error) {
	a.logger.Info("tool call executing", "tool_name", call.Name, "tool_call_id", call.ID)
	a.logger.Debug("tool call input", "tool_name", call.Name, "tool_call_id", call.ID, "input", llm.Truncate(string(call.Input), 500))
	defer func() {
		if recovered := recover(); recovered != nil {
			a.logger.Error("tool panicked", "tool_name", call.Name, "tool_call_id", call.ID, "panic", recovered)
			result = tool.NewErrorResult("tool %q panicked: %v", call.Name, recovered)
			err = nil
		}
	}()
	result, err = t.Execute(ctx, call.Input)
	if err != nil {
		return nil, fmt.Errorf("execute: %w", err)
	}
	if result == nil {
		return tool.NewErrorResult("tool %q returned nil result", call.Name), nil
	}
	a.logger.Debug("tool call result", "tool_name", call.Name, "tool_call_id", call.ID, "is_error", result.IsError(), "result", llm.Truncate(result.Content, 500))
	return result, nil
}

func (a *Agent) limitedToolError(call llm.ToolUseBlock, format string, args ...any) *tool.ToolResult {
	return a.applyToolResultLimit(call, tool.NewErrorResult(format, args...))
}
