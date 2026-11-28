package agent

import (
	"context"
	"errors"
	"iter"

	"github.com/dailz1/go-agent/llm"
)

// RunOption configures a single Run / RunStream invocation. It is a
// per-call option: it never mutates the Agent and is unrelated to the
// constructor-level Option in agent.New.
type RunOption func(*runConfig)

// ErrRunOptionConflict reports ephemeral run options combined with WithThread.
var ErrRunOptionConflict = errors.New("agent: ephemeral run options conflict with WithThread")

// RunWithMaxIter sets this invocation's maximum number of LLM rounds.
// Values below one normalize to 25. Repeated options: the last one wins.
// Supported for both ephemeral and durable runs.
func RunWithMaxIter(n int) RunOption {
	return func(rc *runConfig) {
		rc.maxIter = n
		rc.maxIterSet = true
	}
}

// RunWithLLMOptions overrides constructor LLM options for an ephemeral run.
// The selected batch is appended after constructor options; later fields win.
// Repeated calls replace the previous run-level batch, including an empty batch.
// The option slice is copied at creation; captured function state remains caller-owned.
// Combining this option with WithThread returns ErrRunOptionConflict.
func RunWithLLMOptions(opts ...llm.Option) RunOption {
	copied := append([]llm.Option(nil), opts...)
	return func(rc *runConfig) {
		rc.llmOpts = copied
		rc.llmOptsSet = true
	}
}

// RunWithSystemPrompt overrides the system prompt for an ephemeral run.
// An empty string is an explicit empty system message.
// Repeated options: the last one wins.
// Combining this option with WithThread returns ErrRunOptionConflict.
func RunWithSystemPrompt(prompt string) RunOption {
	return func(rc *runConfig) {
		rc.systemPrompt = prompt
		rc.systemPromptSet = true
	}
}

// WithThread routes this run onto a durable thread.
//
//	absent            → ephemeral run: the store is never touched,
//	                    even when one is configured (ThreadID stays "").
//	WithThread("")    → kernel-generated thread ID (crypto-random 128-bit,
//	                    rev=0 reservation; exposed via DoneEvent/RunResult).
//	WithThread("t-1") → continue (or create) the named thread; the first
//	                    call on an unknown ID creates it and later calls
//	                    replay its frozen system prompt from the log.
//
// Requires WithStore at construction; otherwise the call fails with
// ErrNoStore before anything executes. Calling on a thread whose last run
// never reached a terminal state fails with ErrRunIncomplete — resume it
// with ResumeThread or explicitly settle its original token with
// SettleThread. Repeated WithThread options: the last one wins (same
// override order as constructor options).
//
// WithThread may be combined with RunWithMaxIter. Combining it with
// RunWithLLMOptions or RunWithSystemPrompt, even with an empty argument,
// fails synchronously with ErrRunOptionConflict. ErrNoStore takes
// precedence when no store is configured; otherwise conflict validation
// precedes thread ID validation and iterator creation.
func WithThread(threadID string) RunOption {
	return func(rc *runConfig) {
		rc.thread = threadID
		rc.threadSet = true
	}
}

// runConfig is the per-call configuration of one Run/RunStream invocation.
// It is created fresh for every call, so it never races with concurrent
// invocations of the same immutable Agent.
type runConfig struct {
	thread          string // "" with threadSet=true means kernel-generated
	threadSet       bool
	maxIter         int
	maxIterSet      bool
	llmOpts         []llm.Option
	llmOptsSet      bool
	systemPrompt    string
	systemPromptSet bool
}

// newRunConfig applies per-call options in order, normalizing them the way
// constructors normalize constructor options: later values win, then invalid
// explicit budgets normalize to the system default.
func newRunConfig(opts []RunOption) *runConfig {
	rc := &runConfig{}
	for _, opt := range opts {
		opt(rc)
	}
	if rc.maxIterSet && rc.maxIter < 1 {
		rc.maxIter = 25
	}
	return rc
}

// runSequence is the single dispatch point shared by Run and RunStream:
// Run folds the returned stream, RunStream returns it directly. Ephemeral
// runs connect straight to runStreamInternal; thread runs go through
// executeSession → runStreamInternal, so the single-execution-path
// invariant is preserved.
func (a *Agent) runSequence(ctx context.Context, input string, rc *runConfig) (iter.Seq2[AgentEvent, error], error) {
	if !rc.threadSet {
		// Ephemeral run: whether a store is configured is irrelevant —
		// the store is never touched.
		return a.runStreamInternal(ctx, a.freshHistory(input, *rc), nil, *rc)
	}
	if a.store == nil {
		return nil, ErrNoStore // synchronous, before validateThreadID
	}
	if rc.llmOptsSet || rc.systemPromptSet {
		return nil, ErrRunOptionConflict
	}
	if rc.thread == "" {
		// Kernel-generated thread: rev=0 reservation, lazy.
		return a.runOnNewThreadStream(ctx, input, *rc), nil
	}
	if err := validateThreadID(rc.thread); err != nil {
		return nil, err // synchronous, before the iterator exists
	}
	return a.threadSession(ctx, rc.thread, input, false, false, *rc), nil // lazy
}

// freshHistory builds the opening history of an ephemeral run: the
// configured system prompt (when set) followed by the user input.
func (a *Agent) freshHistory(input string, rc runConfig) []llm.Message {
	history := make([]llm.Message, 0, 16)
	system := a.system
	if rc.systemPromptSet {
		system = llm.SystemMessage(rc.systemPrompt)
	}
	if system.Content != nil {
		history = append(history, system)
	}
	return append(history, llm.UserMessage(input))
}

// ResumeThread continues the thread's incomplete run without appending a new
// input. An interrupted round (a declaration whose tools may or may not have
// executed) is first resolved durably: every unresolved call receives one
// error tool result reporting the outcome as unknown, and the model decides
// how to proceed.
//
// Failures, cancellation, and consumer abandonment leave ordinary rounds
// resumable. The sole exception is a maxIter terminal skip batch after its
// declaration, skipped results, commit, and Done are durably precommitted:
// later delivery interruption cannot reopen it, so ResumeThread returns
// ErrNothingToResume.
// A settled cancelled run instead returns a read-only Cancelled snapshot,
// without executing the model, tools, callbacks, or compaction.
func (a *Agent) ResumeThread(ctx context.Context, threadID string) (*RunResult, error) {
	if a.store == nil {
		return nil, ErrNoStore
	}
	if err := validateThreadID(threadID); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := acquireThread(a.store, threadID); err != nil {
		return nil, err
	}
	defer releaseThread(a.store, threadID)
	v, err := a.replayThread(ctx, threadID)
	if err != nil {
		return nil, err
	}
	if v.cancelled {
		return &RunResult{
			Cancelled: true,
			ThreadID:  threadID,
			History:   deepCopyMessages(seededHistory(v.system, v.history)),
		}, nil
	}
	sess, history, err := a.prepareSessionLocked(ctx, a.store, threadID, "", true, v)
	if err != nil {
		return nil, err
	}
	return a.foldRunStream(a.executeSession(ctx, sess, history, runConfig{}))
}

// threadSession wraps one persistent run. Setup errors (busy thread,
// incompatible log, persistence failure) flow through the iterator so that
// laziness is preserved for stream consumers; ownership is released
// whenever the iteration ends, however it ends.
func (a *Agent) threadSession(ctx context.Context, threadID string, input string, resume, generated bool, rc runConfig) iter.Seq2[AgentEvent, error] {
	return func(yield func(AgentEvent, error) bool) {
		sess, history, err := a.openSession(ctx, a.store, threadID, input, resume, generated)
		if err != nil {
			yield(nil, err)
			return
		}
		defer sess.release()
		a.executeSession(ctx, sess, history, rc)(yield)
	}
}

func (a *Agent) executeSession(ctx context.Context, sess *persistence, history []llm.Message, rc runConfig) iter.Seq2[AgentEvent, error] {
	return func(yield func(AgentEvent, error) bool) {
		defer func() {
			if a.runExitFn != nil {
				a.runExitFn(sess.settlementToken())
			}
		}()
		inner, err := a.runStreamInternal(ctx, history, sess, rc)
		if err != nil {
			yield(nil, wrapPersistentRunError(sess, err))
			return
		}
		inner(func(event AgentEvent, err error) bool {
			if err != nil {
				return yield(nil, wrapPersistentRunError(sess, err))
			}
			return yield(event, nil)
		})
	}
}

// runOnNewThreadStream runs the loop on a fresh kernel-generated thread
// ID. The generated ID uses rev=0 reservation: only a reservation conflict
// is retried, with a fresh ID; a conflict after reservation means real
// concurrent writes and stops the run.
func (a *Agent) runOnNewThreadStream(ctx context.Context, input string, rc runConfig) iter.Seq2[AgentEvent, error] {
	return func(yield func(AgentEvent, error) bool) {
		for range 3 {
			id, err := newThreadID()
			if err != nil {
				yield(nil, err)
				return
			}
			retry := false
			a.threadSession(ctx, id, input, false, true, rc)(func(ev AgentEvent, err error) bool {
				// For a fresh random ID, both reservation conflicts and busy
				// ownership mean "this ID already names a thread" — retry
				// with a new one. No event has been delivered and no model
				// call made, so the retry cannot duplicate or drop anything.
				var interrupted *RunInterruptedError
				if err != nil && !errors.As(err, &interrupted) &&
					(isReservationConflict(err) || errors.Is(err, ErrThreadBusy)) {
					retry = true
					return false
				}
				return yield(ev, err)
			})
			if !retry {
				return
			}
		}
		yield(nil, errors.New("agent: thread id reservation kept conflicting"))
	}
}
