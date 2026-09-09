package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"log/slog"
	"time"

	"github.com/dailz1/go-agent/pkg/llm"
	"github.com/dailz1/go-agent/pkg/tool"
)

// copyMessages returns a deep-enough copy of src: a new []Message slice where
// each Message's Content []ContentBlock also has its own backing array.
func copyMessages(src []llm.Message) []llm.Message {
	dst := make([]llm.Message, len(src))
	copy(dst, src)
	for i := range dst {
		if len(src[i].Content) > 0 {
			dst[i].Content = make([]llm.ContentBlock, len(src[i].Content))
			copy(dst[i].Content, src[i].Content)
		}
	}
	return dst
}

// ApprovalFunc is called before executing a tool that requires approval.
// Return true to proceed, false to reject and report back to the LLM.
type ApprovalFunc func(info tool.ToolInfo, args json.RawMessage) bool

// Agent runs an autonomous LLM loop: chat → tool call → result → chat, until
// the LLM produces a final text response or the iteration limit is reached.
type Agent struct {
	provider   llm.Provider
	registry   *tool.Registry
	system     llm.Message
	maxIter    int
	approvalFn ApprovalFunc
	logger     *slog.Logger
	llmOpts    []llm.Option
	retryCfg   AgentRetryConfig
	retryCfgSet bool
}

// AgentRetryConfig controls retry behavior for provider calls in Agent.Run and Agent.RunStream.
// Attempt numbers are 1-based total attempts: Attempt=1 is the first try, Attempt=2 is the first retry.
//
// Zero-value semantics: if MaxRetries is 0 and the config was not explicitly set via WithRetryConfig,
// defaults are applied (MaxRetries=3, BaseDelay=500ms). To disable retry entirely, set MaxRetries to -1.
type AgentRetryConfig struct {
	MaxRetries int                                           // number of retries after first attempt. Default: 3. Set to -1 to disable retry.
	BaseDelay  time.Duration                                 // base delay for exponential backoff. Default: 500ms.
	MaxDelay   time.Duration                                 // upper bound for a single backoff delay. Default: 120s.
	OnRetry    func(info RetryInfo) // optional callback invoked on each retry
}

// Option configures an Agent via functional options.
type Option func(*Agent)

// WithSystemPrompt sets the system message prepended to every conversation.
func WithSystemPrompt(prompt string) Option {
	return func(a *Agent) { a.system = llm.SystemMessage(prompt) }
}

// WithMaxIter sets the maximum number of LLM round-trips. Default is 10.
func WithMaxIter(n int) Option {
	return func(a *Agent) { a.maxIter = n }
}

// WithApprovalFn registers a callback invoked before executing any tool whose
// ToolInfo.RequiresApproval is true. If the callback returns false, the tool
// call is rejected and the rejection is fed back to the LLM.
func WithApprovalFn(fn ApprovalFunc) Option {
	return func(a *Agent) { a.approvalFn = fn }
}

// WithLogger sets the structured logger. Defaults to slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(a *Agent) { a.logger = l }
}

// WithLLMOptions sets default LLM options (model, temperature, max tokens, etc.)
// passed to every provider.Chat call.
func WithLLMOptions(opts ...llm.Option) Option {
	return func(a *Agent) { a.llmOpts = opts }
}

func WithRetryConfig(cfg AgentRetryConfig) Option {
	return func(a *Agent) { a.retryCfg = cfg; a.retryCfgSet = true }
}

// RunResult holds the outcome of a single agent.Run call.
type RunResult struct {
	// Message is the final assistant response.
	Message llm.Message
	// History is the full conversation trace including all intermediate
	// tool calls and results.
	History []llm.Message
	// ToolCalls is the total number of tool invocations performed.
	ToolCalls int
	// Truncated is true when the agent hit maxIter without reaching a final
	// text response.
	Truncated bool
	// Retries is the list of all retry attempts that occurred during execution.
	Retries []RetryInfo
	// Usage is the token usage from the last successful LLM call.
	Usage llm.Usage
	// TotalUsage is the cumulative token usage across all iterations.
	TotalUsage llm.Usage
}

// New creates an Agent with the given LLM provider and tool registry.
func New(provider llm.Provider, registry *tool.Registry, opts ...Option) *Agent {
	a := &Agent{
		provider: provider,
		registry: registry,
		maxIter:  10,
		logger:   slog.Default(),
	}
	for _, opt := range opts {
		opt(a)
	}
	if a.maxIter < 1 {
		a.maxIter = 10
	}
	return a
}

// Run executes the agent loop with the given user input.
//
// The loop:
//  1. Send conversation history + tool definitions to the LLM.
//  2. If the response contains no tool calls — return it as the final answer.
//  3. For each tool call: check approval → execute → append ToolResultMessage.
//  4. Repeat from step 1.
//
// Returns an error only for system-level failures (provider unreachable, tool
// Execute returned a Go error). Tool-level errors are fed back to the LLM.
func (a *Agent) Run(ctx context.Context, input string) (*RunResult, error) {
	history := make([]llm.Message, 0, 16)
	if a.system.Content != nil {
		history = append(history, a.system)
	}
	history = append(history, llm.UserMessage(input))
	return a.runInternal(ctx, history)
}

func (a *Agent) runInternal(ctx context.Context, history []llm.Message) (*RunResult, error) {
	tools := a.registry.List()

	a.logger.Info("agent run started",
		"tools_count", len(tools),
	)

	var totalToolCalls int
	var allRetries []RetryInfo
	var totalUsage llm.Usage
	var lastUsage llm.Usage

	for i := 0; i < a.maxIter; i++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		a.logger.Debug("agent iteration",
			"iteration", i,
			"history_length", len(history),
		)

		resp, usage, retries, err := a.retryProviderCall(ctx, func() (*llm.Message, *llm.Usage, error) {
			return a.provider.Chat(ctx, history, tools, a.llmOpts...)
		})
		allRetries = append(allRetries, retries...)
		if err != nil {
			return nil, fmt.Errorf("iteration %d: provider chat: %w", i, err)
		}
		if usage != nil {
			lastUsage = *usage
			totalUsage = totalUsage.Add(lastUsage)
		}

		a.logger.Debug("llm response",
			"iteration", i,
			"response", llm.Truncate(messageText(resp), 500),
		)

		toolUseCalls := extractToolUse(resp)
		if len(toolUseCalls) == 0 {
			history = append(history, *resp)
			a.logger.Debug("agent final response",
				"response", llm.Truncate(messageText(resp), 500),
			)
			a.logger.Info("agent run completed",
				"iterations", i+1,
				"tool_calls", totalToolCalls,
				"truncated", false,
			)
			return &RunResult{
				Message:    *resp,
				History:    copyMessages(history),
				ToolCalls:  totalToolCalls,
				Retries:    allRetries,
				Usage:      lastUsage,
				TotalUsage: totalUsage,
			}, nil
		}

		history = append(history, *resp)

		if i == a.maxIter-1 {
			a.logger.Warn("agent run truncated",
				"iterations", a.maxIter,
				"tool_calls", totalToolCalls,
			)
			return &RunResult{
				Message:    *resp,
				History:    copyMessages(history),
				ToolCalls:  totalToolCalls,
				Truncated:  true,
				Retries:    allRetries,
				Usage:      lastUsage,
				TotalUsage: totalUsage,
			}, nil
		}

		totalToolCalls += len(toolUseCalls)

		for _, call := range toolUseCalls {
			result, err := a.executeTool(ctx, call)
			if err != nil {
				return nil, fmt.Errorf("iteration %d: tool %q: %w", i, call.Name, err)
			}
			history = append(history, llm.ToolResultMessage(call.ID, result))
		}
	}
	return nil, fmt.Errorf("unreachable: agent loop exited without returning (maxIter=%d)", a.maxIter)
}

// RunStream executes the agent loop in streaming mode, yielding events to the
// caller in real-time as they occur during LLM response generation, tool
// invocation, and result processing.
//
// Unlike [Agent.Run], which blocks until the entire loop completes and returns
// a single [RunResult], RunStream returns an [iter.Seq2] that the caller ranges
// over to receive [AgentEvent] values incrementally. This enables use cases such
// as progressive UI rendering, server-sent event proxying, and real-time logging.
//
// # Event types yielded during execution
//
// The iterator may yield the following events, in this general order:
//
//   - [TextDeltaEvent]: incremental text fragment from the LLM (1:1 passthrough
//     from the provider's TextDeltaChunk; not batched).
//   - [ToolCallEvent]: a tool is about to be invoked, carrying the complete call
//     ID, function name, and JSON arguments.
//   - [ToolResultEvent]: a tool execution finished, carrying the [tool.ToolResult]
//     (check Result.IsError() for tool-level errors).
//   - [DoneEvent]: the loop terminated, carrying the same fields as [RunResult]
//     (Message, History, ToolCalls, Truncated). History is a defensive copy.
//
// # Error handling
//
// Errors are yielded through the iterator's error channel (the second value in
// the Seq2 pair), not as the outer return error. The outer error is always nil
// in the current implementation because all provider interactions happen inside
// the iterator closure. Errors are wrapped with iteration context using the same
// style as [Agent.Run]:
//
//	fmt.Errorf("iteration %d: provider chat stream: %w", i, err)
//
// Provider-level errors (e.g. [llm.ErrStreamingNotSupported]), mid-stream errors,
// context cancellation, and tool execution errors all propagate through the
// iterator. The caller should check the error value on each iteration.
//
// # Context cancellation
//
// ctx.Done() is checked at two points per iteration: before calling ChatStream
// and before each tool execution. On cancellation, ctx.Err() is yielded as an
// error and the iterator returns immediately.
//
// # Early exit
//
// If the caller breaks out of the range loop, the yield function returns false
// and the iterator stops immediately. This is safe — no goroutines or resources
// are leaked.
//
// # Multi-round tool loop
//
// When the LLM responds with tool calls (finishReason == "tool_calls"), the
// iterator executes each tool, yields ToolCallEvent + ToolResultEvent pairs,
// appends results to the conversation history, and continues to the next LLM
// round — up to the configured maxIter. If maxIter is exhausted, a final
// DoneEvent with Truncated=true is yielded.
//
// # Relationship to Run
//
// RunStream and [Agent.Run] use separate internal implementations (runStreamInternal
// vs runInternal) because they interact with the provider differently (ChatStream vs
// Chat) and return different types (iterator vs struct). Both produce equivalent
// side effects (history structure, tool execution behavior). RunStream shares
// runStreamInternal with [Agent.RunStreamWithHistory].
func (a *Agent) RunStream(ctx context.Context, input string) (iter.Seq2[AgentEvent, error], error) {
	// Build initial conversation history: optional system prompt + user message.
	// Mirrors the initialization in [Agent.Run].
	history := make([]llm.Message, 0, 16)
	if a.system.Content != nil {
		history = append(history, a.system)
	}
	history = append(history, llm.UserMessage(input))
	return a.runStreamInternal(ctx, history)
}

func (a *Agent) runStreamInternal(ctx context.Context, history []llm.Message) (iter.Seq2[AgentEvent, error], error) {
	tools := a.registry.List()

	a.logger.Info("agent runstream started",
		"tools_count", len(tools),
	)

		var totalToolCalls int
		var streamRetries []RetryInfo
		var totalUsage llm.Usage

	// Return an iterator closure. The body executes lazily — nothing happens
	// until the caller starts ranging over the returned Seq2.
	return func(yield func(AgentEvent, error) bool) {
		for i := 0; i < a.maxIter; i++ {
			// Check for context cancellation before each LLM round-trip.
			select {
			case <-ctx.Done():
				yield(nil, ctx.Err())
				return
			default:
			}

			a.logger.Debug("agent stream iteration",
				"iteration", i,
				"history_length", len(history),
			)

			// Request a streaming response from the provider with retry.
			// Pre-stream errors (429, 5xx, network) are retried with backoff;
			// non-retryable errors and mid-stream errors are yielded immediately.
			maxRetries, baseDelay, maxDelay := a.resolveRetryConfig()
			maxAttempts := maxRetries + 1
			if maxRetries < 0 {
				maxAttempts = 1
			}

			var stream iter.Seq2[llm.Chunk, error]
			var lastRetryErr error
			for attempt := 1; attempt <= maxAttempts; attempt++ {
				stream, lastRetryErr = a.provider.ChatStream(ctx, history, tools, a.llmOpts...)
				if lastRetryErr == nil {
					break
				}
				if ctx.Err() != nil {
					yield(nil, ctx.Err())
					return
				}
				if !llm.IsRetryableError(lastRetryErr) && !llm.IsNetworkError(lastRetryErr) {
					yield(nil, fmt.Errorf("iteration %d: provider chat stream (attempt %d/%d): %w", i, attempt, maxAttempts, lastRetryErr))
					return
				}
				a.logger.Warn("agent retrying stream",
					"attempt", attempt+1,
					"max_attempts", maxAttempts,
					"error", lastRetryErr,
				)
				if attempt < maxAttempts {
					delay := llm.Backoff(baseDelay, maxDelay, attempt)
					info := RetryInfo{
						Attempt:     attempt + 1,
						MaxAttempts: maxAttempts,
						Delay:       delay,
						Reason:      llm.SanitizeRetryReason(lastRetryErr),
						Err:         lastRetryErr,
					}
					streamRetries = append(streamRetries, info)
					if !yield(RetryEvent{RetryInfo: info}, nil) {
						return
					}
					select {
					case <-ctx.Done():
						yield(nil, ctx.Err())
						return
					case <-time.After(delay):
					}
				}
			}
			if lastRetryErr != nil {
				yield(nil, fmt.Errorf("iteration %d: provider chat stream failed after %d attempt(s): %w", i, maxAttempts, lastRetryErr))
				return
			}

			// Accumulate tool call fragments and text from the streaming response.
			// The accumulator handles interleaved multi-tool-call streams via
			// map[int]*callAccum keyed by Index, and sorts output by Index in finish().
			var accum toolCallAccum

			for chunk, chunkErr := range stream {
				if chunkErr != nil {
					yield(nil, chunkErr)
					return
				}

			switch c := chunk.(type) {
			case llm.ReasoningDeltaChunk:
				accum.feed(chunk)
				if !yield(ThinkingDeltaEvent{Text: c.Text}, nil) {
					return
				}
			case llm.TextDeltaChunk:
					accum.feed(chunk)
					if !yield(TextDeltaEvent{Text: c.Text}, nil) {
						return
					}
				case llm.ToolCallStartChunk, llm.ToolCallArgsChunk:
					// Feed both start and args chunks to the accumulator.
					// The accumulator's feed() method dispatches internally:
					//   - ToolCallStartChunk → create new callAccum{ id, name }
					//   - ToolCallArgsChunk → append Delta to args strings.Builder
					accum.feed(chunk)
				case llm.DoneChunk:
				accum.feed(chunk)
				default:
					// Silently ignore unknown chunk types for forward compatibility.
				}
			}

			if u := accum.usage(); u != nil {
				totalUsage = totalUsage.Add(*u)
			}

			// Reconstruct the assistant message from accumulated content.
			// We build it manually (combining textBlocks + toolUseBlocks) rather
			// than using llm.AssistantToolCallMessage(), which discards text blocks
			// when tool calls are present. The model may emit "thinking" text
			// alongside tool calls, and that text must be preserved in history.
			var contentBlocks []llm.ContentBlock
			contentBlocks = append(contentBlocks, accum.reasoningBlocks()...)
			contentBlocks = append(contentBlocks, accum.textBlocks()...)
			toolBlocks, assembleErr := accum.assemble()
			if assembleErr != nil {
				a.logger.Warn("stream response corrupted, aborting iteration",
					"iteration", i,
					"error", assembleErr,
				)
				yield(nil, fmt.Errorf("iteration %d: %w", i, assembleErr))
				return
			}
			for _, b := range toolBlocks {
				contentBlocks = append(contentBlocks, b)
			}

			assistantMsg := llm.Message{Role: llm.RoleAssistant, Content: contentBlocks}
			history = append(history, assistantMsg)

			// Terminal condition: no tool calls were accumulated from the stream.
			// This mirrors Run's logic (which checks len(toolUseCalls) == 0)
			// and avoids depending on finishReason correctness from the provider.
			if len(toolBlocks) == 0 {
				a.logger.Info("agent runstream completed",
					"iterations", i+1,
					"tool_calls", totalToolCalls,
					"truncated", false,
				)
				var lastUsage llm.Usage
				if u := accum.usage(); u != nil {
					lastUsage = *u
				}
				yield(DoneEvent{
					Message:    assistantMsg,
					History:    copyMessages(history),
					ToolCalls:  totalToolCalls,
					Retries:    streamRetries,
					Usage:      lastUsage,
					TotalUsage: totalUsage,
				}, nil)
				return
			}

			// The LLM requested tool execution. On the last iteration, skip
			// execution and yield a truncated DoneEvent — tool results would
			// never be fed back to the LLM since the loop is about to exit.
			if i == a.maxIter-1 {
				a.logger.Warn("agent runstream truncated",
					"iterations", a.maxIter,
					"tool_calls", totalToolCalls,
				)
				var lastUsage llm.Usage
				if u := accum.usage(); u != nil {
					lastUsage = *u
				}
				yield(DoneEvent{
					Message:    assistantMsg,
					History:    copyMessages(history),
					ToolCalls:  totalToolCalls,
					Truncated:  true,
					Retries:    streamRetries,
					Usage:      lastUsage,
					TotalUsage: totalUsage,
				}, nil)
				return
			}

			totalToolCalls += len(toolBlocks)

			for _, call := range toolBlocks {
				// Check for context cancellation before each tool execution.
				select {
				case <-ctx.Done():
					yield(nil, ctx.Err())
					return
				default:
				}

				if !yield(ToolCallEvent{ID: call.ID, Name: call.Name, Args: call.Input}, nil) {
					return
				}

				// executeTool handles: registry lookup, approval check,
				// execution, and nil-result guard. System-level errors
				// (e.g. tool.Execute returned a Go error) are yielded as
				// errors; tool-level errors are wrapped in ToolResult with
				// IsError=true and fed back to the LLM in the next iteration.
				result, err := a.executeTool(ctx, call)
				if err != nil {
					yield(nil, fmt.Errorf("iteration %d: tool %q: %w", i, call.Name, err))
					return
				}

				if !yield(ToolResultEvent{ID: call.ID, Name: call.Name, Result: result}, nil) {
					return
				}

				history = append(history, llm.ToolResultMessage(call.ID, result))
			}

			// Reset accumulator for the next iteration. (It's local to this
			// scope so this is technically unnecessary, but explicit reset makes
			// the intent clear and would matter if the accumulator were reused.)
			accum.reset()
		}
	}, nil
}

// RunWithHistory executes the agent loop with a pre-existing conversation history
// appended with the given user input. The history is defensively copied; the caller's
// slice is not modified. The caller is responsible for including any desired system
// prompt in the history — this method does not inject one.
func (a *Agent) RunWithHistory(ctx context.Context, history []llm.Message, input string) (*RunResult, error) {
	copied := copyMessages(history)
	copied = append(copied, llm.UserMessage(input))
	return a.runInternal(ctx, copied)
}

// RunStreamWithHistory executes the streaming agent loop with a pre-existing
// conversation history appended with the given user input. The history is
// defensively copied; the caller's slice is not modified. The caller is
// responsible for including any desired system prompt in the history — this
// method does not inject one.
func (a *Agent) RunStreamWithHistory(ctx context.Context, history []llm.Message, input string) (iter.Seq2[AgentEvent, error], error) {
	copied := copyMessages(history)
	copied = append(copied, llm.UserMessage(input))
	return a.runStreamInternal(ctx, copied)
}

func (a *Agent) resolveRetryConfig() (maxRetries int, baseDelay, maxDelay time.Duration) {
	maxRetries = a.retryCfg.MaxRetries
	if !a.retryCfgSet || maxRetries == 0 {
		maxRetries = 3
	}
	baseDelay = a.retryCfg.BaseDelay
	if baseDelay <= 0 {
		baseDelay = 500 * time.Millisecond
	}
	maxDelay = a.retryCfg.MaxDelay
	if maxDelay <= 0 {
		maxDelay = llm.DefaultMaxDelay
	}
	return
}

func (a *Agent) retryProviderCall(ctx context.Context, fn func() (*llm.Message, *llm.Usage, error)) (*llm.Message, *llm.Usage, []RetryInfo, error) {
	maxRetries, baseDelay, maxDelay := a.resolveRetryConfig()
	if maxRetries < 0 {
		resp, usage, err := fn()
		return resp, usage, nil, err
	}
	maxAttempts := maxRetries + 1

	var lastErr error
	var retries []RetryInfo
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		resp, usage, err := fn()
		if err == nil {
			return resp, usage, retries, nil
		}
		if ctx.Err() != nil {
			return nil, nil, retries, ctx.Err()
		}
		if !llm.IsRetryableError(err) && !llm.IsNetworkError(err) {
			return nil, nil, retries, err
		}
		lastErr = err
		if attempt < maxAttempts {
			delay := llm.Backoff(baseDelay, maxDelay, attempt)
			a.logger.Warn("agent retrying provider call",
				"attempt", attempt+1,
				"max_attempts", maxAttempts,
				"delay", delay,
				"error", err,
			)
			info := RetryInfo{
				Attempt:     attempt + 1,
				MaxAttempts: maxAttempts,
				Delay:       delay,
				Reason:      llm.SanitizeRetryReason(err),
				Err:         err,
			}
			retries = append(retries, info)
			if a.retryCfg.OnRetry != nil {
				a.retryCfg.OnRetry(info)
			}
			select {
			case <-ctx.Done():
				return nil, nil, retries, ctx.Err()
			case <-time.After(delay):
			}
		}
	}
	return nil, nil, retries, fmt.Errorf("provider call failed after %d attempts: %w", maxAttempts, lastErr)
}

func (a *Agent) executeTool(ctx context.Context, call llm.ToolUseBlock) (result *tool.ToolResult, err error) {
	t, ok := a.registry.Get(call.Name)
	if !ok {
		return tool.NewErrorResult("tool %q not found", call.Name), nil
	}

	if t.Info().RequiresApproval && a.approvalFn != nil {
		a.logger.Warn("tool requires approval",
			"tool_name", call.Name,
			"tool_call_id", call.ID,
		)
		if !a.approvalFn(t.Info(), call.Input) {
			a.logger.Warn("tool execution rejected",
				"tool_name", call.Name,
				"tool_call_id", call.ID,
			)
			return tool.NewErrorResult("tool %q execution rejected by user", call.Name), nil
		}
	}

	a.logger.Info("tool call executing",
		"tool_name", call.Name,
		"tool_call_id", call.ID,
	)
	a.logger.Debug("tool call input",
		"tool_name", call.Name,
		"tool_call_id", call.ID,
		"input", llm.Truncate(string(call.Input), 500),
	)

	defer func() {
		if r := recover(); r != nil {
			a.logger.Error("tool panicked",
				"tool_name", call.Name,
				"tool_call_id", call.ID,
				"panic", r,
			)
			result = tool.NewErrorResult("tool %q panicked: %v", call.Name, r)
		}
	}()

	result, err = t.Execute(ctx, call.Input)
	if err != nil {
		return nil, fmt.Errorf("execute: %w", err)
	}
	if result == nil {
		return tool.NewErrorResult("tool %q returned nil result", call.Name), nil
	}

	a.logger.Debug("tool call result",
		"tool_name", call.Name,
		"tool_call_id", call.ID,
		"is_error", result.IsError(),
		"result", llm.Truncate(result.Content, 500),
	)

	return result, nil
}

func extractToolUse(msg *llm.Message) []llm.ToolUseBlock {
	var calls []llm.ToolUseBlock
	for _, block := range msg.Content {
		if tb, ok := block.(llm.ToolUseBlock); ok {
			calls = append(calls, tb)
		}
	}
	return calls
}

func messageText(msg *llm.Message) string {
	var text string
	for _, block := range msg.Content {
		if tb, ok := block.(llm.TextBlock); ok {
			text += tb.Text
		}
	}
	return text
}
