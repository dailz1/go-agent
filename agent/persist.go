package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
)

// persistence is the per-run durability session for one thread. It lives for
// exactly one runStreamInternal invocation; hooks return store failures that
// abort the round (fail-safe: the model never advances past a write that did
// not land). All methods are called from the single run goroutine.
type persistence struct {
	a         *Agent
	thread    string
	runID     string
	system    string
	head      int64
	roundBase int // round index offset for resumed runs
	lastRound int // highest durable round index of this run
	open      int // round index of the open declaration, -1 when none
}

// openSession validates, acquires, replays, and prepares the initial
// history. A caller-supplied thread replays its durable state and appends
// the new run at the current head. A kernel-generated thread uses rev=0
// reservation semantics: if the ID already names a thread, the reservation
// conflicts and the caller retries with a fresh ID — an existing thread is
// never contaminated. For a resume the interrupted round is durably closed
// with outcome-unknown results before returning.
func (a *Agent) openSession(ctx context.Context, st store.Store, thread, input string, resume, generated bool) (*persistence, []llm.Message, error) {
	// Ownership always precedes any store write: the registry is the
	// serialization point for the replay/create sequence below, so nothing
	// is ever written outside an owned session.
	if err := acquireThread(st, thread); err != nil {
		return nil, nil, err
	}
	if generated {
		sess, history, err := a.reserveGeneratedThread(ctx, st, thread, input)
		if err != nil {
			// A reservation conflict means the generated ID already names a
			// thread; the caller retries with a fresh ID. Nothing was written.
			releaseThread(st, thread)
			return nil, nil, err
		}
		return sess, history, nil
	}
	sess, history, err := a.openSessionLocked(ctx, st, thread, input, resume)
	if err != nil {
		releaseThread(st, thread)
		return nil, nil, err
	}
	return sess, history, nil
}

func (a *Agent) openSessionLocked(ctx context.Context, st store.Store, thread, input string, resume bool) (*persistence, []llm.Message, error) {
	v, err := a.replayThread(ctx, thread)
	if err != nil {
		return nil, nil, err
	}
	if resume {
		if !v.runActive {
			return nil, nil, ErrNothingToResume
		}
		if v.open != nil {
			open := *v.open // closeUnknown clears v.open; capture first
			results := v.closeUnknown()
			rec, err := encodeRecord(store.KindRoundCommitted, recordID(open.runID, "-c"+strconv.Itoa(open.round)),
				roundCommittedPayload{RunID: open.runID, Round: open.round, Results: results})
			if err != nil {
				return nil, nil, err
			}
			if _, err := st.Append(ctx, thread, v.head, rec); err != nil {
				return nil, nil, fmt.Errorf("agent: persist recovery commit: %w", err)
			}
			v.head++
		}
		sess := &persistence{a: a, thread: thread, runID: v.runID, system: v.system, head: v.head, roundBase: v.lastRound + 1, lastRound: v.lastRound, open: -1}
		return sess, seededHistory(v.system, v.history), nil
	}
	if v.runActive {
		return nil, nil, fmt.Errorf("%w: run %s", ErrRunIncomplete, v.runID)
	}
	system := v.system
	if !v.systemSet {
		system = promptText(a.system)
	}
	runID, err := newRunID()
	if err != nil {
		return nil, nil, err
	}
	rec, err := encodeRecord(store.KindRunStarted, recordID(runID, "-start"),
		runStartedPayload{RunID: runID, Input: input, SystemPrompt: system})
	if err != nil {
		return nil, nil, err
	}
	if _, err := st.Append(ctx, thread, v.head, rec); err != nil {
		return nil, nil, fmt.Errorf("agent: persist run start on thread %q: %w", thread, err)
	}
	sess := &persistence{a: a, thread: thread, runID: runID, system: system, head: v.head + 1, roundBase: 0, lastRound: -1, open: -1}
	return sess, seededHistory(system, append(v.history, llm.UserMessage(input))), nil
}

// reserveGeneratedThread reserves a kernel-generated thread ID at revision
// zero. A conflict means the generated ID already names a thread: it is
// reported as a reservation conflict (the caller retries with a fresh ID)
// and the existing thread is never touched.
func (a *Agent) reserveGeneratedThread(ctx context.Context, st store.Store, thread, input string) (*persistence, []llm.Message, error) {
	system := promptText(a.system)
	runID, err := newRunID()
	if err != nil {
		return nil, nil, err
	}
	rec, err := encodeRecord(store.KindRunStarted, recordID(runID, "-start"),
		runStartedPayload{RunID: runID, Input: input, SystemPrompt: system})
	if err != nil {
		return nil, nil, err
	}
	// Establish nonexistence explicitly: under ownership, this process has
	// no concurrent writer for the thread, and the run ID is fresh random
	// content, so an idempotent match cannot arise. A cross-process writer
	// surfaces as ErrRevisionConflict — v1 is single-process by contract.
	state, err := st.Latest(ctx, thread)
	if err != nil {
		return nil, nil, fmt.Errorf("agent: reserve run on thread %q: %w", thread, err)
	}
	if state.Head != 0 {
		return nil, nil, &reservationConflictError{ThreadID: thread,
			Err: fmt.Errorf("thread already exists at head %d", state.Head)}
	}
	// The reservation append is retried byte-identical so an ambiguous
	// failure resolves through Store idempotency: if the store reports an
	// idempotent match — the record already landed — the existing reservation
	// is adopted and the run continues under the same thread and run ID; if
	// it reports a content mismatch, that is a reservation conflict. No error
	// path can strand an active run on an undiscoverable thread — if every
	// attempt fails, the run ID is returned in the error so the caller can
	// still ResumeThread it.
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if ctx.Err() != nil {
			return nil, nil, fmt.Errorf("agent: reserve run on thread %q: %w", thread, ctx.Err())
		}
		if _, err := st.Append(ctx, thread, 0, rec); err != nil {
			if errors.Is(err, store.ErrRevisionConflict) {
				return nil, nil, &reservationConflictError{ThreadID: thread, Err: err}
			}
			lastErr = err
			continue
		}
		sess := &persistence{a: a, thread: thread, runID: runID, system: system, head: 1, roundBase: 0, lastRound: -1, open: -1}
		return sess, seededHistory(system, []llm.Message{llm.UserMessage(input)}), nil
	}
	return nil, nil, fmt.Errorf("agent: reserve run on thread %q inconclusive after retries (run %s may be active; resume it): %w", thread, runID, lastErr)
}

// append stores records at the session's expected revision.
func (p *persistence) append(ctx context.Context, records ...store.Record) error {
	head, err := p.a.store.Append(ctx, p.thread, p.head, records...)
	if err != nil {
		return fmt.Errorf("agent: persist %s on thread %q: %w", records[0].Kind, p.thread, err)
	}
	p.head = head
	return nil
}

// declareRound durably records the round's complete assistant message before
// any tool call is announced, executed, or sent to approval.
func (p *persistence) declareRound(ctx context.Context, round int, msg llm.Message) error {
	r := p.roundBase + round
	rec, err := encodeRecord(store.KindRoundDeclared, recordID(p.runID, "-d"+strconv.Itoa(r)),
		roundDeclaredPayload{RunID: p.runID, Round: r, Message: msg})
	if err != nil {
		return err
	}
	if err := p.append(ctx, rec); err != nil {
		return err
	}
	p.open = r
	if r > p.lastRound {
		p.lastRound = r
	}
	return nil
}

// commitRound durably closes the round with its ordered results.
func (p *persistence) commitRound(ctx context.Context, round int, results []llm.Message) error {
	r := p.roundBase + round
	if p.open != r {
		return errors.New("agent: commit without an open declaration")
	}
	rec, err := encodeRecord(store.KindRoundCommitted, recordID(p.runID, "-c"+strconv.Itoa(r)),
		roundCommittedPayload{RunID: p.runID, Round: r, Results: results})
	if err != nil {
		return err
	}
	p.open = -1
	return p.append(ctx, rec)
}

// truncateRun closes a maxIter-truncated round: the declared calls are known
// not to have executed, so they get deterministic skipped results — not
// "outcome unknown" — and the run is closed with a truncated Done whose
// message is empty (the declared assistant message is already in history).
func (p *persistence) truncateRun(ctx context.Context, round int, msg llm.Message, toolCalls int, usage, total llm.Usage) ([]llm.Message, error) {
	if err := p.declareRound(ctx, round, msg); err != nil {
		return nil, err
	}
	results := iterationLimitResults(toolUseBlocks(msg))
	if err := p.commitRound(ctx, round, results); err != nil {
		return nil, err
	}
	// The truncated Done carries an assistant-role message with no content:
	// the declared message is already in history and must not repeat.
	if err := p.finishRun(ctx, llm.Message{Role: llm.RoleAssistant}, toolCalls, true, usage, total); err != nil {
		return nil, err
	}
	return results, nil
}

// finishRun durably records the terminal Done state before the event may be
// yielded.
func (p *persistence) finishRun(ctx context.Context, msg llm.Message, toolCalls int, truncated bool, usage, total llm.Usage) error {
	inner, err := json.Marshal(doneEventPayload{Message: msg, ToolCalls: toolCalls, Truncated: truncated, Usage: usage, TotalUsage: total})
	if err != nil {
		return fmt.Errorf("agent: encode done payload: %w", err)
	}
	rec, err := encodeRecord(store.KindAgentEvent, recordID(p.runID, "-done"),
		eventEnvelope{Type: "done", Version: eventEnvelopeV1, Payload: inner})
	if err != nil {
		return err
	}
	return p.append(ctx, rec)
}
