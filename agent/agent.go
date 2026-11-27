package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"log/slog"
	"math"
	"reflect"
	"strings"
	"sync"
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
		if src[i].Content != nil {
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
			case *llm.ReasoningItemBlock:
				if typed != nil {
					cloned := *typed
					cloned.Summary = append([]string(nil), cloned.Summary...)
					dst[messageIndex].Content[blockIndex] = &cloned
				}
			case *llm.ToolResultBlock:
				if typed != nil {
					cloned := *typed
					cloned.Images = cloneImageBlocks(cloned.Images)
					dst[messageIndex].Content[blockIndex] = &cloned
				}
			case *llm.ToolUseBlock:
				if typed != nil {
					cloned := *typed
					cloned.Input = bytes.Clone(typed.Input)
					dst[messageIndex].Content[blockIndex] = &cloned
				}
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

// clonableBlock reports whether a content block's concrete type is deep-copy
// safe: handled by the deepCopyMessages switch. Only pointer forms satisfy
// the sealed interface (issue #24), so one case per variant covers every
// representable block. Keep this whitelist in sync with the
// deepCopyMessages switch.
func clonableBlock(block llm.ContentBlock) bool {
	switch block.(type) {
	case *llm.TextBlock, *llm.ReasoningBlock, *llm.ReasoningItemBlock,
		*llm.ToolResultBlock, *llm.ToolUseBlock, *llm.ImageBlock:
		return true
	default:
		return false
	}
}

// hasUnsupportedBlocks reports whether history contains block types outside
// the clonable whitelist. Two consumers: run-entry validation rejects such
// histories outright (RunWithHistory / RunStreamWithHistory, issue #25), and
// the compaction boundary rejects compactor results containing them. The
// check on compaction input is gone — entry validation guarantees built-in
// blocks only. Keep this whitelist in sync with the deepCopyMessages switch.
func hasUnsupportedBlocks(history []llm.Message) bool {
	for i := range history {
		for _, block := range history[i].Content {
			if !clonableBlock(block) {
				return true
			}
		}
	}
	return false
}

// cloneImageBlocks deep-copies a ToolResultBlock's image attachments so the
// cloned history shares no backing arrays with the caller's slices.
func cloneImageBlocks(images []llm.ImageBlock) []llm.ImageBlock {
	if images == nil {
		return nil
	}
	out := make([]llm.ImageBlock, len(images))
	for i, img := range images {
		img.Data = bytes.Clone(img.Data)
		out[i] = img
	}
	return out
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
	providers                  map[string]llm.Provider
	altProviders               []llm.Provider
	registry                   *tool.Registry
	system                     llm.Message
	maxIter                    int
	contextWindowTokens        int
	compactor                  Compactor
	overflowRecovery           bool
	compactionThresholdPercent int
	approvalFn                 ApprovalFunc
	logger                     *slog.Logger
	llmOpts                    []llm.Option
	retryCfg                   AgentRetryConfig
	retryCfgSet                bool
	store                      store.Store
	toolConcurrency            int
	dynamicTools               bool
	roundContextProvider       RoundContextProvider
	finishRound                func(ctx context.Context, fc FinishRoundContext) (FinishRoundDecision, error)
	runExitFn                  func(SettlementToken)

	warnMu                  sync.Mutex
	warnedUndeclaredWindows map[string]struct{}
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

// WithOverflowRecovery controls bounded recovery from provider-confirmed
// context overflow. It is enabled by default, including on resumed threads.
func WithOverflowRecovery(enabled bool) Option {
	return func(a *Agent) { a.overflowRecovery = enabled }
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

// WithDynamicTools refreshes the advertised tool snapshot at the top of every
// iteration, between the context check and compaction, including iteration
// zero; the same snapshot serves every retry attempt and the streaming of one
// iteration. Without this option the snapshot stays fixed at run start for
// the whole run, per the P2-2 contract. See docs/DESIGN.md (P2-4).
func WithDynamicTools() Option {
	return func(a *Agent) { a.dynamicTools = true }
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

// WithProviders registers alternate providers for cross-provider hot
// switching on durable threads. The positional provider of New stays the
// default and is registered under its own Name; every provider passed
// here is keyed by its Name, and a later registration with the same name
// replaces the earlier one (last write wins, no fail-fast — distinguish
// same-named instances by overriding Name). A thread whose folded
// configuration names a provider missing from the table fails the round
// with ErrUnknownProvider. Anonymous runs always use the default.
func WithProviders(providers ...llm.Provider) Option {
	return func(a *Agent) { a.altProviders = append(a.altProviders, providers...) }
}

// maxRetryCap bounds user-configured MaxRetries at the largest value whose
// surrounding +1 arithmetic (maxAttempts, attempt+1 in logs) cannot
// overflow; it preserves "retry as much as representable" without imposing
// a smaller policy cap.
const maxRetryCap = math.MaxInt - 2

func WithRetryConfig(cfg AgentRetryConfig) Option {
	// Clamp overflow-prone budgets at the option constructor (issue #16):
	// maxRetryCap is the largest value whose surrounding +1 arithmetic
	// (maxAttempts, attempt+1 in logs) cannot overflow. It preserves
	// "retry as much as representable" without imposing a smaller policy
	// cap; resolveRetryConfig needs no second guard.
	if cfg.MaxRetries > maxRetryCap {
		cfg.MaxRetries = maxRetryCap
	}
	return func(a *Agent) { a.retryCfg = cfg; a.retryCfgSet = true }
}

// WithStore configures the store that durable threads live in. Run /
// RunStream persist only when the call passes WithThread; ResumeThread /
// SettleThread / SettlementTarget address threads explicitly. Without a
// store nothing changes: no thread ID is generated and the persistence path
// never executes. See docs/DESIGN.md for the persistence contract.
func WithStore(s store.Store) Option {
	return func(a *Agent) { a.store = s }
}

// WithRoundContextProvider enables a transient runtime-context overlay on
// every direct provider invocation. A nil provider disables the feature.
func WithRoundContextProvider(provider RoundContextProvider) Option {
	return func(a *Agent) { a.roundContextProvider = provider }
}

// RunResult holds the outcome of a single agent.Run call.
type RunResult struct {
	// Cancelled identifies a read-only ResumeThread snapshot of a settled run.
	// Message and execution statistics are zero in this case.
	Cancelled bool
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
		overflowRecovery:           true,
		compactionThresholdPercent: DefaultCompactionThresholdPercent,
		logger:                     slog.Default(),
		toolConcurrency:            1,
		warnedUndeclaredWindows:    make(map[string]struct{}),
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
	if a.roundContextProvider == nil {
		a.roundContextProvider = nil
	}
	// The provider table pairs with New's positional default: the default
	// is registered under its own Name first, WithProviders entries follow,
	// and a later same-name registration replaces the earlier entry.
	a.providers = make(map[string]llm.Provider, 1+len(a.altProviders))
	if provider != nil {
		a.providers[provider.Name()] = provider
	}
	for _, p := range a.altProviders {
		if p != nil {
			a.providers[p.Name()] = p
		}
	}
	a.altProviders = nil
	return a
}

// Run executes the agent loop with the given user input. Without options
// the run is ephemeral: it never touches a configured store and ThreadID
// stays ""; pass WithThread to route it onto a durable thread.
//
// The loop:
//  1. Send conversation history + tool definitions to the LLM.
//  2. If the response contains no tool calls — return it as the final answer.
//  3. For each tool call: check approval → execute → append ToolResultMessage.
//  4. Repeat from step 1.
//
// Returns an error only for system-level failures (provider unreachable, tool
// Execute returned a Go error). Tool-level errors are fed back to the LLM.
func (a *Agent) Run(ctx context.Context, input string, opts ...RunOption) (*RunResult, error) {
	seq, err := a.runSequence(ctx, input, newRunConfig(opts))
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
// Without options the run is ephemeral; WithThread routes it onto a durable
// thread, whose stream stays lazy: ownership, replay, and the input commit
// happen on the first iteration, so a never-ranged call has no observable
// effect, and events of previous runs are never re-emitted.
//
// See docs/DESIGN.md for design detail.
func (a *Agent) RunStream(ctx context.Context, input string, opts ...RunOption) (iter.Seq2[AgentEvent, error], error) {
	return a.runSequence(ctx, input, newRunConfig(opts))
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
		accounting := &contextRuntime{durable: sess != nil}
		if sess != nil {
			sess.context = accounting
			accounting.restoreAnchor(sess.anchor, history)
			defer func() { sess.context = nil }()
		}
		for i := 0; i < a.maxIter; i++ {
			// Check for context cancellation before each LLM round-trip.
			select {
			case <-ctx.Done():
				yield(nil, ctx.Err())
				return
			default:
			}
			if a.dynamicTools {
				tools = a.registry.List()
			}
			var ok bool
			// Per-round fixed order: read config → compact → drain → budget → request.
			var opts []llm.Option
			prov := a.provider
			if sess != nil {
				if sess.recovery != nil && !a.overflowRecovery {
					recovery := overflowRecovery{state: sess.recovery}
					yield(nil, recovery.failure("disabled", nil))
					return
				}
				if err := sess.refreshConfig(ctx); err != nil {
					yield(nil, err)
					return
				}
				if err := sess.anchorLegacyHistory(ctx, history); err != nil {
					yield(nil, err)
					return
				}
				opts = sess.cfg.apply(a.llmOpts)
				// Provider resolution reads the same projection and shares the
				// config pipeline: empty folded name = the thread never switched
				// (default provider); a named provider resolves through the
				// injected table and stays fixed for the whole round — every
				// retry attempt and the stream fallback reuse the instance.
				if name := sess.cfg.Provider; name != nil && *name != "" {
					p, ok := a.providers[*name]
					if !ok {
						yield(nil, fmt.Errorf("agent: thread %q names provider %q which WithProviders did not inject: %w", threadID, *name, ErrUnknownProvider))
						return
					}
					prov = p
				}
			} else {
				opts = a.llmOpts
			}
			opts = freezeLLMOptions(opts)
			measurement := requestMeasurement(prov, opts, tools)
			accounting.checkIdentity(measurement)
			w := a.roundContextWindow(prov, opts)
			recovery := overflowRecovery{a: a, sess: sess, provider: prov, opts: opts,
				window: w, round: i + sessionRoundBase(sess), cfgSeq: -1, context: accounting,
				measurement: measurement}
			if sess != nil {
				recovery.state, recovery.cfgSeq = sess.recovery, sess.cfgSeq
			}
			if a.overflowRecovery {
				if err := recovery.selectConfig(ctx, history); err != nil {
					yield(nil, err)
					return
				}
			}
			if recovery.state == nil {
				history, ok = a.compactHistoryRound(ctx, history, prov, opts, yield, sess, w, accounting)
				if !ok {
					return
				}
			}
			if sess != nil && recovery.state == nil {
				// D1 drain: queued steering inputs inject here, after
				// compaction, so they ride into this round's request. The
				// drain consumes only what fits the hard limit; the rest
				// stays queued for the next round.
				injected, err := sess.drainInputs(ctx, func() int {
					capacity := uint64(a.steeringAdmissionRunesFor(w))
					estimated := accounting.estimate(history)
					return int(capacity - min(capacity, estimated))
				})
				if err != nil {
					yield(nil, err)
					return
				}
				history = append(history, injected...)
			}
			if recovery.state == nil && a.roundContextProvider != nil && accounting.estimate(history) > uint64(a.canonicalContextCapFor(w)) {
				yield(nil, fmt.Errorf("canonical history exceeds round context budget: %w", ErrCompactionBudgetExceeded))
				return
			}

			a.logger.Debug("agent stream iteration",
				"iteration", i,
				"history_length", len(history),
			)
			accum, ok := recovery.receiveRound(
				ctx,
				&history,
				tools,
				RoundContextRequest{
					Round:    i + sessionRoundBase(sess),
					ThreadID: threadID,
				},
				yield,
				&streamRetries,
			)
			if !ok {
				return
			}
			if u := accum.usage(); u != nil {
				totalUsage = totalUsage.Add(*u)
			}
			assistantMsg, toolBlocks, ok := a.assembleAssistantMessage(accum, i, yield)
			if !ok {
				return
			}
			if sess != nil {
				if err := sess.checkRecoveryRound(i + sessionRoundBase(sess)); err != nil {
					yield(nil, err)
					return
				}
			}
			history = append(history, assistantMsg)
			recovery.measurement.captureConfigSeq = recovery.cfgSeq
			accounting.activateMeasurement(recovery.measurement, accum.usage(), history, sess)
			isLast := i == a.maxIter-1
			finishEnd := false
			declaredText := false

			// Terminal condition: no tool calls were accumulated from the
			// stream — unless a FinishRound Continue keeps the run alive,
			// in which case the turn is durably declared as terminal_text
			// before the hook is consulted (D8a).
			if len(toolBlocks) == 0 {
				if a.finishRound != nil && !isLast {
					if sess != nil {
						if err := sess.declareTerminalRound(ctx, i, assistantMsg); err != nil {
							yield(nil, err)
							return
						}
					}
					declaredText = true
					decision, ferr := a.callFinishRound(ctx, FinishRoundContext{
						ThreadID:  threadID,
						Round:     i + sessionRoundBase(sess),
						Assistant: copyMessage(assistantMsg),
					})
					if ferr != nil {
						yield(nil, ferr)
						return
					}
					if decision.Action == FinishRoundContinue {
						// The declaration is durable; the loop continues so
						// the queued steering inputs (if any) ride into the
						// guaranteed next request.
						continue
					}
					if decision.Action == FinishRoundEnd {
						finishEnd = true
					}
					// Default with a hook configured: the declaration is
					// already durable, so the natural done below closes via
					// run_closed instead of re-carrying the message.
				}

				a.logger.Info("agent runstream completed",
					"iterations", i+1,
					"tool_calls", totalToolCalls,
					"truncated", false,
				)
				var lastUsage llm.Usage
				if u := accum.usage(); u != nil {
					lastUsage = *u
				}
				if sess != nil {
					if declaredText || finishEnd {
						// The declared terminal_text turn already carries the
						// assistant message; run_closed closes the run the
						// cooperative way without re-carrying it.
						if err := sess.closeRun(ctx, totalToolCalls, lastUsage, totalUsage); err != nil {
							yield(nil, err)
							return
						}
					} else if err := sess.finishRun(ctx, assistantMsg, totalToolCalls, false, lastUsage, totalUsage); err != nil {
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
					if !yield(ToolCallEvent{ID: call.ID, Name: call.Name, Args: bytes.Clone(call.Input)}, nil) {
						return
					}
				}
				for index, call := range toolBlocks {
					block := skipped[index].Content[0].(*llm.ToolResultBlock)
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
				if !yield(ToolCallEvent{ID: call.ID, Name: call.Name, Args: bytes.Clone(call.Input)}, nil) {
					return
				}
			}
			if !a.executeToolRound(ctx, i, toolBlocks, &history, sess, yield, w) {
				return
			}
			// FinishRound runs after the round's results are committed, with
			// the trailing tool results visible in history.
			if a.finishRound != nil {
				decision, ferr := a.callFinishRound(ctx, FinishRoundContext{
					ThreadID:  threadID,
					Round:     i + sessionRoundBase(sess),
					Assistant: copyMessage(assistantMsg),
					Results:   trailingToolResults(history),
				})
				if ferr != nil {
					yield(nil, ferr)
					return
				}
				if decision.Action == FinishRoundEnd {
					// D7: close via run_closed — the declared assistant and
					// its results are already durable, no message is invented.
					a.logger.Info("agent runstream cooperatively closed",
						"iterations", i+1,
					)
					var lastUsage llm.Usage
					if u := accum.usage(); u != nil {
						lastUsage = *u
					}
					if sess != nil {
						if err := sess.closeRun(ctx, totalToolCalls, lastUsage, totalUsage); err != nil {
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
				// Continue adds nothing here: a tool round always continues.
			}
			accum.reset()
		}
	}, nil
}

// roundContextWindow resolves the effective context window for one round
// (issue #17): min(constructor C, provider-declared M) per docs/DESIGN.md
// P1「模型上下文窗口声明与有效窗口」. An undeclared or non-positive
// declaration falls back to C with a warn-once log naming the provider and
// model. W is round-local: callers pass it to every budget derivation for
// the round and never store it on the Agent.
func (a *Agent) roundContextWindow(prov llm.Provider, opts []llm.Option) int {
	if prov == nil {
		return a.contextWindowTokens
	}
	model := llm.ApplyOptions(opts).Model
	declared, ok := 0, false
	if cp, isCP := prov.(llm.ContextWindowProvider); isCP {
		declared, ok = cp.ContextWindow(model)
	}
	if ok && declared > 0 {
		if declared < a.contextWindowTokens {
			return declared
		}
		return a.contextWindowTokens
	}
	// Issue #23: attribute the warning to the provider-reported default
	// model when the round carries no selector. Diagnostics only — the
	// empty selector still went to ContextWindow and requests stay
	// untouched.
	warningModel := model
	if warningModel == "" {
		if reporter, isReporter := prov.(llm.DefaultModelReporter); isReporter {
			warningModel = reporter.DefaultModelName()
		}
	}
	a.warnUndeclaredWindow(prov.Name(), warningModel)
	return a.contextWindowTokens
}

// warnUndeclaredWindow logs the fallback once per provider+model pair per
// Agent (all runs and threads of one Agent share the table), so hosts can
// see which models run unprotected. model is the final warning name: the
// round's selector, or the provider-reported default when the selector is
// empty (issue #23).
func (a *Agent) warnUndeclaredWindow(providerName, model string) {
	key := providerName + "\x00" + model
	a.warnMu.Lock()
	if _, seen := a.warnedUndeclaredWindows[key]; seen {
		a.warnMu.Unlock()
		return
	}
	a.warnedUndeclaredWindows[key] = struct{}{}
	window := a.contextWindowTokens
	a.warnMu.Unlock()
	a.logger.Warn("provider context window undeclared; using constructor-level window",
		"provider", providerName,
		"model", model,
		"window_tokens", window,
	)
}

// targetRunesFor is the compaction target for the round's effective window:
// floor(w*percent/100) with the same overflow-safe idiom as toolResultRunes,
// capped by the canonical cap under a runtime-context overlay.
func (a *Agent) targetRunesFor(w int) int {
	target := (w/100)*a.compactionThresholdPercent +
		(w%100)*a.compactionThresholdPercent/100
	if a.roundContextProvider != nil && target > a.canonicalContextCapFor(w) {
		target = a.canonicalContextCapFor(w)
	}
	return target
}

// steeringAdmissionRunesFor is the hard history limit an enqueued steering
// input can never exceed, measured serialized like every other budget: the
// round's effective window, or the canonical cap under a runtime-context
// overlay (whose reservation must survive steering injection). Fit at the
// actual drain point is enforced by deferral, not by admission.
func (a *Agent) steeringAdmissionRunesFor(w int) int {
	if a.roundContextProvider != nil {
		return a.canonicalContextCapFor(w)
	}
	return w
}

// compactHistoryRound enforces the history budget at the top of each round and reports via events.
// provider and opts are the round's already-resolved effective provider and
// options: the summarization strategy follows them, so a thread that hot
// switched providers summarizes on the switched-to provider with the
// thread's effective model.
func (a *Agent) compactHistoryRound(
	ctx context.Context,
	history []llm.Message,
	provider llm.Provider,
	opts []llm.Option,
	yield func(AgentEvent, error) bool,
	sess *persistence,
	w int,
	accounting *contextRuntime,
) ([]llm.Message, bool) {
	// Same overflow-safe idiom as toolResultRunes: floor(w*percent/100).
	targetRunes := a.targetRunesFor(w)
	estimated := accounting.estimate(history)
	anchored := accounting.anchor != nil
	budgetRunes := targetRunes
	beforeRunes := 0
	triggered := a.compactor != nil && estimated > uint64(targetRunes)
	if triggered {
		beforeRunes = int(estimated)
		if anchored {
			beforeRunes = estimateRunes(history)
			budgetRunes = int(compactionTargetRunes(int64(targetRunes), int64(beforeRunes), estimated))
		}
	}
	usageTokens, tailTokens := int64(0), estimated
	if anchor := accounting.anchor; anchor != nil {
		usageTokens = anchor.inputTokens + anchor.outputTokens
		tailTokens = uint64(anchor.suffixRunes)
	}
	fields := []any{"estimate_tokens", estimated, "anchor_valid", accounting.anchor != nil,
		"usage_anchor_tokens", usageTokens, "tail_estimate_tokens", tailTokens}
	if accounting.invalidationReason != "" {
		fields = append(fields, "invalidation_reason", accounting.invalidationReason)
		accounting.invalidationReason = ""
	}
	if triggered {
		fields = append(fields, "budget_runes", budgetRunes)
	}
	a.logger.Debug("agent context budget", fields...)
	if triggered {
		if anchored && budgetRunes <= 0 {
			yield(nil, fmt.Errorf("history exceeds compaction budget: %w", ErrCompactionBudgetExceeded))
			return history, false
		}
		// The compactor is user code: hand it an isolated deep copy so
		// in-place mutation cannot leak into the live history (issue #9).
		// Validation below compares against the untouched original. The former
		// input-side foreign-block skip is gone: run-entry validation (issue
		// #25) rejects such histories before the loop, so the input here is
		// built-in blocks only.
		compactInput := deepCopyMessages(history)
		result, err := a.compactor.Compact(ctx, compactInput, CompactionBudget{MaxRunes: budgetRunes}, provider, append([]llm.Option(nil), opts...))
		// Decide rejection on the compactor's ORIGINAL return value. Durable
		// normalization below round-trips the result through JSON, which would
		// launder a wrapper block whose wire form matches a built-in type into
		// *llm.TextBlock and defeat the scan. Recovery applies the same check
		// before canonicalization; the two paths must stay in the same order
		// (issue #25 review, #33).
		resultRejected := err == nil && ((result.Changed && len(result.History) == 0) || hasUnsupportedBlocks(result.History))
		if err == nil && result.Changed && !resultRejected && sess != nil {
			var canonical []llm.Message
			canonical, err = durableHistory(history)
			if err == nil {
				result.History, err = durableHistory(result.History)
			}
			if err == nil {
				err = validateCompacted(canonical, result.History)
			}
		}
		for _, strategyErr := range result.Errors {
			a.logger.Warn("history compaction strategy failed", "error", strategyErr)
		}
		if err != nil {
			a.logger.Warn("history compaction failed", "error", err)
		} else if resultRejected {
			a.logger.Warn("history compaction produced empty or unsupported history; ignoring compaction result")
		} else if validationErr := validateCompacted(history, result.History); validationErr != nil {
			a.logger.Warn("history compaction produced invalid history", "error", validationErr)
		} else {
			if err := ctx.Err(); err != nil {
				yield(nil, err)
				return history, false
			}
			candidateEstimate := estimated
			if result.Changed {
				candidateEstimate = uint64(estimateRunes(result.History))
			}
			// Only a verified strategy result proves the history cannot
			// fit. Check it before replacing history or announcing it.
			// Steering carry remains bounded by the hard window.
			if anchored || a.roundContextProvider == nil {
				carry := 0
				if sess != nil {
					carry = sess.carry
				}
				limit := targetRunes + min(carry, w-targetRunes)
				runeLimit := budgetRunes + min(carry, w-budgetRunes)
				if (result.Changed && candidateEstimate > uint64(runeLimit)) || candidateEstimate > uint64(limit) {
					yield(nil, fmt.Errorf("history exceeds compaction budget: %w", ErrCompactionBudgetExceeded))
					return history, false
				}
			}
			if result.Changed {
				adopted := deepCopyMessages(result.History)
				// The compacted view is durable before it is announced, so
				// replay never re-runs a (nondeterministic) summarizer.
				if sess != nil {
					id, err := newRunID()
					if err == nil {
						adopted, err = sess.replaceHistory(ctx, history, historyReplacedPayload{
							RunID: sess.runID, Round: sess.lastRound + 1, OperationID: id,
							Reason: "proactive_compaction", History: adopted,
						})
					}
					if err != nil {
						yield(nil, err)
						return history, false
					}
				}
				if sess == nil {
					accounting.replaceHistory()
				}
				afterRunes := int(candidateEstimate)
				estimated = uint64(afterRunes)
				if sess != nil {
					if err := sess.checkpoint(ctx, adopted); err != nil {
						yield(nil, err)
						return history, false
					}
				}
				history = adopted
				a.logger.Warn("history compacted",
					"strategies", strings.Join(result.Strategies, ","),
					"dropped_groups", result.DroppedGroups,
					"before_runes", beforeRunes,
					"after_runes", afterRunes,
				)
				if result.SpanTruncated {
					a.logger.Warn("summarization span truncated to fit budget",
						"span_total_runes", result.SpanTotalRunes,
						"span_kept_runes", result.SpanKeptRunes,
					)
				}
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
		}
		// Runs on every compaction outcome: a context canceled during the
		// compactor call must not reach the provider through a Warn-and-
		// continue path (issue #9 review).
		select {
		case <-ctx.Done():
			yield(nil, ctx.Err())
			return history, false
		default:
		}
	}
	if a.roundContextProvider != nil && estimated > uint64(a.canonicalContextCapFor(w)) {
		yield(nil, fmt.Errorf("canonical history exceeds round context budget: %w", ErrCompactionBudgetExceeded))
		return history, false
	}
	return history, true
}

func (a *Agent) assembleAssistantMessage(
	accum *toolCallAccum,
	iteration int,
	yield func(AgentEvent, error) bool,
) (llm.Message, []*llm.ToolUseBlock, bool) {
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

// validateRunHistory rejects histories carrying content blocks outside the
// six built-in llm.ContentBlock variants (issue #25). It names the first
// offending block with its position so the failure is actionable at the
// call site instead of surfacing mid-run at the provider boundary or
// aliasing the caller's history through a custom provider.
func validateRunHistory(history []llm.Message) error {
	for i := range history {
		for j, block := range history[i].Content {
			if !clonableBlock(block) {
				return fmt.Errorf("invalid history: message %d block %d (%T): %w", i, j, block, ErrForeignBlock)
			}
		}
	}
	return nil
}

// RunWithHistory executes the agent loop with a pre-existing conversation history
// appended with the given user input. The history is defensively copied; the caller's
// slice is not modified. The caller is responsible for including any desired system
// prompt in the history — this method does not inject one.
//
// The history is validated before the run starts: any content block outside
// the six built-in llm.ContentBlock variants fails the call with an error
// wrapping ErrForeignBlock (issue #25).
func (a *Agent) RunWithHistory(ctx context.Context, history []llm.Message, input string) (*RunResult, error) {
	if err := validateRunHistory(history); err != nil {
		return nil, err
	}
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
//
// Like RunWithHistory, the history is validated before the run starts: any
// content block outside the six built-in llm.ContentBlock variants fails the
// call with an error wrapping ErrForeignBlock (issue #25).
func (a *Agent) RunStreamWithHistory(ctx context.Context, history []llm.Message, input string) (iter.Seq2[AgentEvent, error], error) {
	if err := validateRunHistory(history); err != nil {
		return nil, err
	}
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

func sessionRoundBase(sess *persistence) int {
	if sess == nil {
		return 0
	}
	return sess.roundBase
}

func (a *Agent) toolResultRunesFor(w int) int {
	// (t/10)*3 + (t%10)*3/10 is floor(t*3/10) without the overflow that
	// tokens*3/10 hits at large windows.
	runes := (w/10)*3 + (w%10)*3/10
	if runes < minToolResultRunes {
		runes = minToolResultRunes
	}
	return runes
}

// toolResultRunes is the constructor-window variant, kept for direct
// callers and tests outside a running round.
func (a *Agent) toolResultRunes() int {
	return a.toolResultRunesFor(a.contextWindowTokens)
}

func (a *Agent) applyToolResultLimit(call *llm.ToolUseBlock, result *tool.ToolResult, w int) *tool.ToolResult {
	totalRunes := utf8.RuneCountInString(result.Content)
	capRunes := a.toolResultRunesFor(w)
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

func (a *Agent) executeTool(ctx context.Context, call *llm.ToolUseBlock) (result *tool.ToolResult, err error) {
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
		if tb, ok := block.(*llm.TextBlock); ok {
			text += tb.Text
		}
	}
	return text
}
