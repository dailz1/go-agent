package agent

import (
	"context"
	"errors"
	"iter"
)

// RunThread executes the agent loop on a persistent thread: the input is
// committed to the store before the model is called, every round's tool-call
// declaration is committed before any tool executes, and the terminal state
// is committed before DoneEvent. Calling RunThread on a thread whose last
// run never reached a terminal state fails with ErrRunIncomplete — resume it
// with ResumeThread.
//
// The first call on an unknown thread creates it; the thread's system prompt
// is captured on creation and replayed from the log on every later run,
// regardless of later WithSystemPrompt configuration.
func (a *Agent) RunThread(ctx context.Context, threadID string, input string) (*RunResult, error) {
	if a.store == nil {
		return nil, ErrNoStore
	}
	if err := validateThreadID(threadID); err != nil {
		return nil, err
	}
	return a.foldRunStream(a.threadSession(ctx, threadID, input, false, false))
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
func (a *Agent) ResumeThread(ctx context.Context, threadID string) (*RunResult, error) {
	if a.store == nil {
		return nil, ErrNoStore
	}
	if err := validateThreadID(threadID); err != nil {
		return nil, err
	}
	return a.foldRunStream(a.threadSession(ctx, threadID, "", true, false))
}

// RunThreadStream is the streaming form of RunThread. The returned iterator
// is lazy: ownership, replay, and the input commit happen on the first
// iteration, so a never-ranged call has no observable effect. Events of
// previous runs are never re-emitted; the stream carries only the events of
// this invocation.
func (a *Agent) RunThreadStream(ctx context.Context, threadID string, input string) (iter.Seq2[AgentEvent, error], error) {
	if a.store == nil {
		return nil, ErrNoStore
	}
	if err := validateThreadID(threadID); err != nil {
		return nil, err
	}
	return a.threadSession(ctx, threadID, input, false, false), nil
}

// threadSession wraps one persistent run. Setup errors (busy thread,
// incompatible log, persistence failure) flow through the iterator so that
// laziness is preserved for stream consumers; ownership is released
// whenever the iteration ends, however it ends.
func (a *Agent) threadSession(ctx context.Context, threadID string, input string, resume, generated bool) iter.Seq2[AgentEvent, error] {
	return func(yield func(AgentEvent, error) bool) {
		sess, history, err := a.openSession(ctx, a.store, threadID, input, resume, generated)
		if err != nil {
			yield(nil, err)
			return
		}
		defer sess.release()
		inner, err := a.runStreamInternal(ctx, history, sess)
		if err != nil {
			yield(nil, err)
			return
		}
		inner(yield)
	}
}

// runOnNewThread runs the loop on a fresh kernel-generated thread ID. The
// generated ID uses rev=0 reservation: only a reservation conflict is
// retried, with a fresh ID; a conflict after reservation means real
// concurrent writes and stops the run. The stream form shares the retry so
// Run and RunStream behave identically.
func (a *Agent) runOnNewThread(ctx context.Context, input string) (*RunResult, error) {
	return a.foldRunStream(a.runOnNewThreadStream(ctx, input))
}

func (a *Agent) runOnNewThreadStream(ctx context.Context, input string) iter.Seq2[AgentEvent, error] {
	return func(yield func(AgentEvent, error) bool) {
		for range 3 {
			id, err := newThreadID()
			if err != nil {
				yield(nil, err)
				return
			}
			retry := false
			a.threadSession(ctx, id, input, false, true)(func(ev AgentEvent, err error) bool {
				// For a fresh random ID, both reservation conflicts and busy
				// ownership mean "this ID already names a thread" — retry
				// with a new one. No event has been delivered and no model
				// call made, so the retry cannot duplicate or drop anything.
				if err != nil && (isReservationConflict(err) || errors.Is(err, ErrThreadBusy)) {
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
