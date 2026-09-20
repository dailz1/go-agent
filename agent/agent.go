package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
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

// deepCopyMessages also isolates pointer-form blocks and their mutable byte slices.
func deepCopyMessages(src []llm.Message) []llm.Message {
	dst := copyMessages(src)
	for messageIndex := range dst {
		for blockIndex, block := range dst[messageIndex].Content {
			switch typed := block.(type) {
			case *llm.TextBlock:
				if typed != nil {
					cloned := *typed
					dst[messageIndex].Content[blockIndex] = &cloned
				}
			case *llm.ReasoningBlock:
				if typed != nil {
					cloned := *typed
					dst[messageIndex].Content[blockIndex] = &cloned
				}
			case llm.ReasoningItemBlock:
				typed.Summary = append([]string(nil), typed.Summary...)
				dst[messageIndex].Content[blockIndex] = typed
			case *llm.ReasoningItemBlock:
				if typed != nil {
					cloned := *typed
					cloned.Summary = append([]string(nil), cloned.Summary...)
					dst[messageIndex].Content[blockIndex] = &cloned
				}
			case *llm.ToolResultBlock:
				if typed != nil {
					cloned := *typed
					dst[messageIndex].Content[blockIndex] = &cloned
				}
			case llm.ToolUseBlock:
				typed.Input = bytes.Clone(typed.Input)
				dst[messageIndex].Content[blockIndex] = typed
			case *llm.ToolUseBlock:
				if typed != nil {
					cloned := *typed
					cloned.Input = bytes.Clone(typed.Input)
					dst[messageIndex].Content[blockIndex] = &cloned
				}
			case llm.ImageBlock:
				typed.Data = bytes.Clone(typed.Data)
				dst[messageIndex].Content[blockIndex] = typed
			case *llm.ImageBlock:
				if typed != nil {
					cloned := *typed
					cloned.Data = bytes.Clone(typed.Data)
					dst[messageIndex].Content[blockIndex] = &cloned
				}
			}
		}
	}
	return dst
}

func validateCompacted(original []llm.Message, compacted []llm.Message) error {
	originalGroups, err := partitionHistory(original)
	if err != nil {
		return err
	}
	compactedGroups, err := partitionHistory(compacted)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(systemPrefix(originalGroups), systemPrefix(compactedGroups)) {
		return fmt.Errorf("system prefix changed: %w", ErrInvalidHistory)
	}
	return nil
}

// ApprovalFunc is called before executing a tool that requires approval.
// Return true to proceed, false to reject and report back to the LLM.
type ApprovalFunc func(info tool.ToolInfo, args json.RawMessage) bool

// Agent runs an autonomous LLM loop: chat → tool call → result → chat, until
// the LLM produces a final text response or the iteration limit is reached.
type Agent struct {
	provider                   llm.Provider
	registry                   *tool.Registry
	system                     llm.Message
	maxIter                    int
	contextWindowTokens        int
	compactor                  Compactor
	compactionThresholdPercent int
	approvalFn                 ApprovalFunc
	logger                     *slog.Logger
	llmOpts                    []llm.Option
	retryCfg                   AgentRetryConfig
	retryCfgSet                bool
	store                      store.Store
	toolConcurrency            int
}

// AgentRetryConfig controls retry behavior for provider calls in Agent.Run and Agent.RunStream.
// Attempt numbers are 1-based total attempts: Attempt=1 is the first try, Attempt=2 is the first retry.
//
// Zero-value semantics: if MaxRetries is 0 and the config was not explicitly set via WithRetryConfig,
// defaults are applied (MaxRetries=3, BaseDelay=500ms). To disable retry entirely, set MaxRetries to -1.
type AgentRetryConfig struct {
	MaxRetries int                  // number of retries after first attempt. Default: 3. Set to -1 to disable retry.
	BaseDelay  time.Duration        // base delay for exponential backoff. Default: 500ms.
	MaxDelay   time.Duration        // upper bound for a single backoff delay. Default: 120s.
	OnRetry    func(info RetryInfo) // optional callback invoked on each retry
}

// DefaultContextWindowTokens is the assumed context window. Callers should set
// the model's real value; math.MaxInt effectively disables tool result
// truncation.
const DefaultContextWindowTokens = 8_192

// DefaultCompactionThresholdPercent is the percent of the configured context
// window where compaction triggers. Values outside [1, 100] normalize to this default.
const DefaultCompactionThresholdPercent = 80

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

// WithContextWindowTokens sets the model's context window size. Tool results
// are limited to a rune approximation of 30% of the window. Values less than
// or equal to zero use [DefaultContextWindowTokens].
func WithContextWindowTokens(tokens int) Option {
	return func(a *Agent) { a.contextWindowTokens = tokens }
}

// WithCompactor sets the history compactor. A nil compactor disables compaction.
func WithCompactor(c Compactor) Option {
	return func(a *Agent) { a.compactor = c }
}

// WithCompactionThresholdPercent sets the context-window percentage where
// compaction triggers. Values outside [1, 100] normalize to
// [DefaultCompactionThresholdPercent].
func WithCompactionThresholdPercent(percent int) Option {
	return func(a *Agent) { a.compactionThresholdPercent = percent }
}

// WithApprovalFn registers a callback invoked before executing any tool whose
// ToolInfo.RequiresApproval is true. If the callback returns false, the tool
// call is rejected and the rejection is fed back to the LLM.
func WithApprovalFn(fn ApprovalFunc) Option {
	return func(a *Agent) { a.approvalFn = fn }
}

// WithToolConcurrency sets the maximum number of tool calls dispatched in
// parallel within one model response. Values less than one normalize to one.
// The default is one, preserving serial execution semantics.
func WithToolConcurrency(n int) Option {
	return func(a *Agent) { a.toolConcurrency = n }
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

// WithStore enables kernel-automatic session persistence: RunThread /
// ResumeThread / RunThreadStream address threads explicitly, and Run / RunStream
// gain a kernel-generated thread ID exposed via ThreadID fields. Without a
// store nothing changes: no thread ID is generated and the persistence path
// never executes. See docs/DESIGN.md for the persistence contract.
func WithStore(s store.Store) Option {
	return func(a *Agent) { a.store = s }
}

// RunResult holds the outcome of a single agent.Run call.
type RunResult struct {
	// Message is the final assistant response.
	Message llm.Message
	// History is the full conversation trace including all intermediate
	// tool calls and results.
	History []llm.Message
	// ToolCalls is the total number of tool calls the model requested
	// (announced calls, including rejected or unknown tools).
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
	// ThreadID identifies the persisted thread when a store is configured;
	// empty without one.
	ThreadID string
}

// New creates an Agent with the given LLM provider and tool registry.
func New(provider llm.Provider, registry *tool.Registry, opts ...Option) *Agent {
	a := &Agent{
		provider:                   provider,
		registry:                   registry,
		maxIter:                    10,
		contextWindowTokens:        DefaultContextWindowTokens,
		compactor:                  NewStandardCompactor(),
		compactionThresholdPercent: DefaultCompactionThresholdPercent,
		logger:                     slog.Default(),
		toolConcurrency:            1,
	}
	for _, opt := range opts {
		opt(a)
	}
	if a.maxIter < 1 {
		a.maxIter = 10
	}
	if a.contextWindowTokens <= 0 {
		a.contextWindowTokens = DefaultContextWindowTokens
	}
	if a.compactionThresholdPercent < 1 || a.compactionThresholdPercent > 100 {
		a.compactionThresholdPercent = DefaultCompactionThresholdPercent
	}
	if a.toolConcurrency <= 0 {
		a.toolConcurrency = 1
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
	if a.store != nil {
		return a.runOnNewThread(ctx, input)
	}
	history := make([]llm.Message, 0, 16)
	if a.system.Content != nil {
		history = append(history, a.system)
	}
	history = append(history, llm.UserMessage(input))
	seq, err := a.runStreamInternal(ctx, history, nil)
	if err != nil {
		return nil, err
	}
	return a.foldRunStream(seq)
}

func (a *Agent) foldRunStream(seq iter.Seq2[AgentEvent, error]) (*RunResult, error) {
	var retries []RetryInfo
	for event, err := range seq {
		if err != nil {
			return nil, err
		}

		switch e := event.(type) {
		case RetryEvent:
			retries = append(retries, e.RetryInfo)
		case DoneEvent:
			return &RunResult{
				Message:    e.Message,
				History:    e.History,
				ToolCalls:  e.ToolCalls,
				Truncated:  e.Truncated,
				Retries:    retries,
				Usage:      e.Usage,
				TotalUsage: e.TotalUsage,
				ThreadID:   e.ThreadID,
			}, nil
		case TextDeltaEvent, ThinkingDeltaEvent, ToolCallEvent, ToolResultEvent, CompactionEvent:
			// Run returns only the completed result.
		default:
			return nil, fmt.Errorf("agent stream emitted unknown event %T", event)
		}
	}
	return nil, fmt.Errorf("agent stream ended without a done event")
}

// RunStream executes the agent loop in streaming mode. It returns a lazy
// [iter.Seq2] of [AgentEvent] values; errors (provider, tool, cancellation)
// flow through the iterator's second value and stop iteration. Breaking out
// of the range early is safe. The loop runs up to the configured maxIter
// rounds; the final [DoneEvent] carries the same fields as [RunResult].
//
// See docs/DESIGN.md for design detail.
func (a *Agent) RunStream(ctx context.Context, input string) (iter.Seq2[AgentEvent, error], error) {
	if a.store != nil {
		return a.runOnNewThreadStream(ctx, input), nil
	}
	history := make([]llm.Message, 0, 16)
	if a.system.Content != nil {
		history = append(history, a.system)
	}
	history = append(history, llm.UserMessage(input))
	return a.runStreamInternal(ctx, history, nil)
}

func (a *Agent) runStreamInternal(ctx context.Context, history []llm.Message, sess *persistence) (iter.Seq2[AgentEvent, error], error) {
	tools := a.registry.List()
	a.logger.Info("agent runstream started", "tools_count", len(tools))

	var totalToolCalls int
	var streamRetries []RetryInfo
	var totalUsage llm.Usage
	var threadID string
	if sess != nil {
		threadID = sess.thread
	}

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
			var ok bool
			history, ok = a.compactHistoryRound(ctx, history, yield, sess)
			if !ok {
				return
			}

			a.logger.Debug("agent stream iteration",
				"iteration", i,
				"history_length", len(history),
			)
			stream, ok := a.chatWithRetryAndFallback(
				ctx,
				history,
				tools,
				i,
				yield,
				&streamRetries,
			)
			if !ok {
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
				case llm.ReasoningItemChunk:
					// Entry-level reasoning item (Responses protocol): consumed
					// by the accumulator for ordered assembly; not a consumer
					// event.
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
			assistantMsg, toolBlocks, ok := a.assembleAssistantMessage(&accum, i, yield)
			if !ok {
				return
			}
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
				// Terminal state is durable before the event may be delivered.
				if sess != nil {
					if err := sess.finishRun(ctx, assistantMsg, totalToolCalls, false, lastUsage, totalUsage); err != nil {
						yield(nil, err)
						return
					}
				}
				yield(DoneEvent{
					Message:    assistantMsg,
					History:    copyMessages(history),
					ToolCalls:  totalToolCalls,
					Retries:    streamRetries,
					Usage:      lastUsage,
					TotalUsage: totalUsage,
					ThreadID:   threadID,
				}, nil)
				return
			}
			// The LLM requested tools. At the iteration limit, declare every request
			// and record a deterministic skipped result without approval or execution.
			// A Store closes that terminal batch before its TC/TR/Done delivery.
			if i == a.maxIter-1 {
				if err := ctx.Err(); err != nil {
					yield(nil, err)
					return
				}
				totalToolCalls += len(toolBlocks)
				a.logger.Warn("agent runstream truncated",
					"iterations", a.maxIter,
					"tool_calls", totalToolCalls,
				)
				var lastUsage llm.Usage
				if u := accum.usage(); u != nil {
					lastUsage = *u
				}
				var skipped []llm.Message
				if sess != nil {
					var err error
					skipped, err = sess.truncateRun(ctx, i, assistantMsg, totalToolCalls, lastUsage, totalUsage)
					if err != nil {
						yield(nil, err)
						return
					}
				} else {
					skipped = iterationLimitResults(toolBlocks)
				}
				history = append(history, skipped...)
				for _, call := range toolBlocks {
					if !yield(ToolCallEvent{ID: call.ID, Name: call.Name, Args: call.Input}, nil) {
						return
					}
				}
				for index, call := range toolBlocks {
					block := skipped[index].Content[0].(llm.ToolResultBlock)
					result := &tool.ToolResult{Content: block.Content}
					if block.IsError {
						result.Status = tool.ResultError
					}
					if !yield(ToolResultEvent{ID: call.ID, Name: call.Name, Result: result}, nil) {
						return
					}
				}
				yield(DoneEvent{
					Message:    assistantMsg,
					History:    copyMessages(history),
					ToolCalls:  totalToolCalls,
					Truncated:  true,
					Retries:    streamRetries,
					Usage:      lastUsage,
					TotalUsage: totalUsage,
					ThreadID:   threadID,
				}, nil)
				return
			}

			totalToolCalls += len(toolBlocks)
			// The whole round is durable before any call is announced,
			// executed, or sent to approval.
			if sess != nil {
				if err := sess.declareRound(ctx, i, assistantMsg); err != nil {
					yield(nil, err)
					return
				}
			}
			// Announce every call of the round before executing any of them.
			// Contiguous ToolCallEvents mark one assistant reply, which is
			// what lets consumers rebuild history from events alone; do not
			// add a context check inside this batch — splitting it would
			// leave an appended assistant message partially announced and
			// impossible to reconstruct.
			for _, call := range toolBlocks {
				if !yield(ToolCallEvent{ID: call.ID, Name: call.Name, Args: call.Input}, nil) {
					return
				}
			}
			if !a.executeToolRound(ctx, i, toolBlocks, &history, sess, yield) {
				return
			}
			accum.reset()
		}
	}, nil
}

// compactHistoryRound enforces the history budget at the top of each round and reports via events.
func (a *Agent) compactHistoryRound(
	ctx context.Context,
	history []llm.Message,
	yield func(AgentEvent, error) bool,
	sess *persistence,
) ([]llm.Message, bool) {
	// Same overflow-safe idiom as toolResultRunes: floor(windowTokens*percent/100).
	targetRunes := (a.contextWindowTokens/100)*a.compactionThresholdPercent +
		(a.contextWindowTokens%100)*a.compactionThresholdPercent/100
	beforeRunes := estimateRunes(history)
	if a.compactor != nil && beforeRunes > targetRunes {
		result, err := a.compactor.Compact(ctx, history, CompactionBudget{MaxRunes: targetRunes})
		for _, strategyErr := range result.Errors {
			a.logger.Warn("history compaction strategy failed", "error", strategyErr)
		}
		if err != nil {
			a.logger.Warn("history compaction failed", "error", err)
		} else if validationErr := validateCompacted(history, result.History); validationErr != nil {
			a.logger.Warn("history compaction produced invalid history", "error", validationErr)
		} else {
			if result.Changed {
				history = result.History
				afterRunes := estimateRunes(history)
				// The compacted view is durable before it is announced, so
				// replay never re-runs a (nondeterministic) summarizer.
				if sess != nil {
					if err := sess.checkpoint(ctx, history); err != nil {
						yield(nil, err)
						return history, false
					}
				}
				a.logger.Warn("history compacted",
					"strategies", strings.Join(result.Strategies, ","),
					"dropped_groups", result.DroppedGroups,
					"before_runes", beforeRunes,
					"after_runes", afterRunes,
				)
				if !yield(CompactionEvent{
					Strategies:    append([]string{}, result.Strategies...),
					DroppedGroups: result.DroppedGroups,
					BeforeRunes:   beforeRunes,
					AfterRunes:    afterRunes,
					History:       deepCopyMessages(history),
				}, nil) {
					return history, false
				}
			}

			select {
			case <-ctx.Done():
				yield(nil, ctx.Err())
				return history, false
			default:
			}
			if estimateRunes(history) > targetRunes {
				yield(nil, fmt.Errorf("history exceeds compaction budget: %w", ErrCompactionBudgetExceeded))
				return history, false
			}
		}
	}
	return history, true
}

func (a *Agent) assembleAssistantMessage(
	accum *toolCallAccum,
	iteration int,
	yield func(AgentEvent, error) bool,
) (llm.Message, []llm.ToolUseBlock, bool) {
	// Reconstruct the assistant message from accumulated content. We build
	// it manually (combining textBlocks + toolUseBlocks) rather than using
	// llm.AssistantToolCallMessage(), which discards text blocks when tool
	// calls are present. The model may emit "thinking" text alongside tool
	// calls, and that text must be preserved in history.
	contentBlocks, toolBlocks, assembleErr := accum.assembleContent()
	if assembleErr != nil {
		a.logger.Warn("stream response corrupted, aborting iteration",
			"iteration", iteration,
			"error", assembleErr,
		)
		yield(nil, fmt.Errorf("iteration %d: %w", iteration, assembleErr))
		return llm.Message{}, nil, false
	}

	assistantMsg := llm.Message{Role: llm.RoleAssistant, Content: contentBlocks}
	return assistantMsg, toolBlocks, true
}

func (a *Agent) chatWithRetryAndFallback(
	ctx context.Context,
	history []llm.Message,
	tools []tool.ToolInfo,
	iteration int,
	yield func(AgentEvent, error) bool,
	streamRetries *[]RetryInfo,
) (iter.Seq2[llm.Chunk, error], bool) {
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
		if errors.Is(lastRetryErr, llm.ErrStreamingNotSupported) {
			response, usage, err := a.provider.Chat(ctx, history, tools, a.llmOpts...)
			lastRetryErr = err
			if err == nil {
				stream = func(yield func(llm.Chunk, error) bool) {
					for _, chunk := range synthesizeChunks(response.Content, usage) {
						if !yield(chunk, nil) {
							return
						}
					}
				}
				break
			}
		}
		if lastRetryErr == nil {
			break
		}
		if ctx.Err() != nil {
			yield(nil, ctx.Err())
			return nil, false
		}
		if !llm.IsRetryableError(lastRetryErr) && !llm.IsNetworkError(lastRetryErr) {
			yield(nil, fmt.Errorf("iteration %d: provider chat stream (attempt %d/%d): %w", iteration, attempt, maxAttempts, lastRetryErr))
			return nil, false
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
			*streamRetries = append(*streamRetries, info)
			if !yield(RetryEvent{RetryInfo: info}, nil) {
				return nil, false
			}
			if a.retryCfg.OnRetry != nil {
				a.retryCfg.OnRetry(info)
			}
			if werr := llm.WaitForRetry(ctx, lastRetryErr, baseDelay, maxDelay, attempt); werr != nil {
				yield(nil, werr)
				return nil, false
			}
		}
	}
	if lastRetryErr != nil {
		yield(nil, fmt.Errorf("iteration %d: provider chat stream failed after %d attempt(s): %w", iteration, maxAttempts, lastRetryErr))
		return nil, false
	}
	return stream, true
}

func synthesizeChunks(blocks []llm.ContentBlock, usage *llm.Usage) []llm.Chunk {
	chunks := make([]llm.Chunk, 0, len(blocks)+1)
	hasToolUse := false
	for outputIndex, block := range blocks {
		switch b := block.(type) {
		case llm.ReasoningBlock:
			chunks = append(chunks, llm.ReasoningDeltaChunk{Text: b.Content})
		case llm.TextBlock:
			chunks = append(chunks, llm.TextDeltaChunk{Text: b.Text, OutputIndex: int64(outputIndex)})
		case llm.ToolUseBlock:
			hasToolUse = true
			chunks = append(chunks,
				llm.ToolCallStartChunk{Index: outputIndex, ID: b.ID, Name: b.Name},
				llm.ToolCallArgsChunk{Index: outputIndex, Delta: string(b.Input)},
			)
		case llm.ReasoningItemBlock:
			chunks = append(chunks, llm.ReasoningItemChunk{OutputIndex: int64(outputIndex), Item: b})
		case llm.ImageBlock, llm.ToolResultBlock:
			// These blocks have no streaming representation.
		}
	}
	finishReason := "stop"
	if hasToolUse {
		finishReason = "tool_calls"
	}
	return append(chunks, llm.DoneChunk{FinishReason: finishReason, Usage: usage})
}

// RunWithHistory executes the agent loop with a pre-existing conversation history
// appended with the given user input. The history is defensively copied; the caller's
// slice is not modified. The caller is responsible for including any desired system
// prompt in the history — this method does not inject one.
func (a *Agent) RunWithHistory(ctx context.Context, history []llm.Message, input string) (*RunResult, error) {
	copied := deepCopyMessages(history)
	copied = append(copied, llm.UserMessage(input))
	seq, err := a.runStreamInternal(ctx, copied, nil)
	if err != nil {
		return nil, err
	}
	return a.foldRunStream(seq)
}

// RunStreamWithHistory executes the streaming agent loop with a pre-existing
// conversation history appended with the given user input. The history is
// defensively copied; the caller's slice is not modified. The caller is
// responsible for including any desired system prompt in the history — this
// method does not inject one.
func (a *Agent) RunStreamWithHistory(ctx context.Context, history []llm.Message, input string) (iter.Seq2[AgentEvent, error], error) {
	copied := deepCopyMessages(history)
	copied = append(copied, llm.UserMessage(input))
	return a.runStreamInternal(ctx, copied, nil)
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

func (a *Agent) toolResultRunes() int {
	// (t/10)*3 + (t%10)*3/10 is floor(t*3/10) without the overflow that
	// tokens*3/10 hits at large windows.
	runes := (a.contextWindowTokens/10)*3 + (a.contextWindowTokens%10)*3/10
	if runes < minToolResultRunes {
		runes = minToolResultRunes
	}
	return runes
}

func (a *Agent) applyToolResultLimit(call llm.ToolUseBlock, result *tool.ToolResult) *tool.ToolResult {
	totalRunes := utf8.RuneCountInString(result.Content)
	capRunes := a.toolResultRunes()
	limitedResult, omittedRunes := truncateToolResult(result, capRunes)
	if omittedRunes > 0 {
		a.logger.Warn("tool result truncated",
			"tool_name", call.Name,
			"tool_call_id", call.ID,
			"omitted_runes", omittedRunes,
			"total_runes", totalRunes,
			"cap_runes", capRunes,
		)
	}
	return limitedResult
}

func (a *Agent) executeTool(ctx context.Context, call llm.ToolUseBlock) (result *tool.ToolResult, err error) {
	t, ok := a.registry.Get(call.Name)
	if !ok {
		return tool.NewErrorResult("tool %q not found", call.Name), nil
	}

	if t.Info().RequiresApproval {
		if a.approvalFn == nil {
			return tool.NewErrorResult("tool %q requires approval but no approval callback is configured", call.Name), nil
		}
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

	return a.executeToolHandle(ctx, call, t)
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
