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
	roundBase int          // round index offset for resumed runs
	lastRound int          // highest durable round index of this run
	open      int          // round index of the open declaration, -1 when none
	cfg       configState  // effective runtime config, folded from replay + absorbed tails
	queue     []queueEntry // pending steering inputs, FIFO from replay + absorbed tails
	carry     int          // runes the last drain injected past the compaction target
	cfgRead   int64        // next seq to scan for control records at round tops
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
	return a.prepareSessionLocked(ctx, st, thread, input, resume, v)
}

func (a *Agent) prepareSessionLocked(ctx context.Context, st store.Store, thread, input string, resume bool, v threadView) (*persistence, []llm.Message, error) {
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
			head, err := a.appendRebaseable(ctx, st, thread, v.head, &v.cfg, &v.queue, rec)
			if err != nil {
				return nil, nil, fmt.Errorf("agent: persist recovery commit: %w", err)
			}
			v.head = head
		}
		sess := &persistence{a: a, thread: thread, runID: v.runID, system: v.system, head: v.head, roundBase: v.lastRound + 1, lastRound: v.lastRound, open: -1, cfg: v.cfg, queue: v.queue, cfgRead: v.head}
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
	head, err := a.appendRebaseable(ctx, st, thread, v.head, &v.cfg, &v.queue, rec)
	if err != nil {
		return nil, nil, fmt.Errorf("agent: persist run start on thread %q: %w", thread, err)
	}
	sess := &persistence{a: a, thread: thread, runID: runID, system: system, head: head, roundBase: 0, lastRound: -1, open: -1, cfg: v.cfg, queue: v.queue, cfgRead: head}
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
		sess := &persistence{a: a, thread: thread, runID: runID, system: system, head: 1, roundBase: 0, lastRound: -1, open: -1, cfgRead: 1}
		return sess, seededHistory(system, []llm.Message{llm.UserMessage(input)}), nil
	}
	return nil, nil, fmt.Errorf("agent: reserve run on thread %q inconclusive after retries (run %s may be active; resume it): %w", thread, runID, lastErr)
}

// append stores records at the session's expected revision. On a revision
// conflict an external control-plane writer (config_changed) has advanced
// the log: the tail is re-read from the last known head, control records
// are folded into the session projection, and the append retries at the
// true head — the D6(a) absorb protocol. Any non-control record in the gap
// is unabsorbable and fails loudly.
func (p *persistence) append(ctx context.Context, records ...store.Record) error {
	const maxAttempts = 4
	var err error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			if aerr := p.absorbTail(ctx, p.head, true); aerr != nil {
				return fmt.Errorf("agent: persist %s on thread %q: %w", records[0].Kind, p.thread, aerr)
			}
		}
		var head int64
		head, err = p.a.store.Append(ctx, p.thread, p.head, records...)
		if err == nil {
			p.head = head
			if head > p.cfgRead {
				p.cfgRead = head
			}
			return nil
		}
		if !errors.Is(err, store.ErrRevisionConflict) {
			break
		}
	}
	return fmt.Errorf("agent: persist %s on thread %q: %w", records[0].Kind, p.thread, err)
}

// refreshConfig folds any config_changed records appended since the last
// scan into the session's effective config. Called at each round top.
func (p *persistence) refreshConfig(ctx context.Context) error {
	return p.absorbTail(ctx, p.cfgRead, false)
}

// absorbTail reads the log from seq and folds config_changed records into
// the session projection. In strict mode (write-point conflict) any other
// record in the gap is a hard error; in scan mode (round top) the gap holds
// this session's own lifecycle records and only config records are folded.
func (p *persistence) absorbTail(ctx context.Context, from int64, strict bool) error {
	recs, err := p.a.store.History(ctx, p.thread, from)
	if err != nil {
		return fmt.Errorf("agent: absorb control records on thread %q: %w", p.thread, err)
	}
	high := from
	for _, r := range recs {
		switch {
		case r.Kind == store.KindConfigChanged:
			cp, derr := decodeConfigChanged(r)
			if derr != nil {
				return derr
			}
			p.cfg.fold(cp)
		case r.Kind == store.KindInputQueued:
			qp, derr := decodeInputQueued(r)
			if derr != nil {
				return derr
			}
			p.queue = append(p.queue, queueEntry{ID: r.ID, Input: qp.Input})
		case strict:
			return fmt.Errorf("agent: unabsorbable record %s at seq %d on thread %q: %w", r.Kind, r.Seq, p.thread, store.ErrRevisionConflict)
		}
		if r.Seq+1 > high {
			high = r.Seq + 1
		}
	}
	if high > p.cfgRead {
		p.cfgRead = high
	}
	if high > p.head {
		p.head = high
	}
	return nil
}

// appendRebaseable appends fresh records at head, rebasing across external
// control-plane writes by folding config_changed records into cfg. The
// records must carry fresh IDs and content that does not depend on the log
// position; lifecycle-position-bound payloads (checkpoints) use their own
// re-serializing loops instead.
func (a *Agent) appendRebaseable(ctx context.Context, st store.Store, thread string, head int64, cfg *configState, queue *[]queueEntry, records ...store.Record) (int64, error) {
	const maxAttempts = 4
	var err error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			recs, rerr := st.History(ctx, thread, head)
			if rerr != nil {
				return 0, fmt.Errorf("agent: absorb control records on thread %q: %w", thread, rerr)
			}
			high := head
			for _, r := range recs {
				switch r.Kind {
				case store.KindConfigChanged:
					cp, derr := decodeConfigChanged(r)
					if derr != nil {
						return 0, derr
					}
					cfg.fold(cp)
				case store.KindInputQueued:
					qp, derr := decodeInputQueued(r)
					if derr != nil {
						return 0, derr
					}
					*queue = append(*queue, queueEntry{ID: r.ID, Input: qp.Input})
				default:
					return 0, fmt.Errorf("agent: unabsorbable record %s at seq %d on thread %q: %w", r.Kind, r.Seq, thread, store.ErrRevisionConflict)
				}
				if r.Seq+1 > high {
					high = r.Seq + 1
				}
			}
			head = high
		}
		var applied int64
		applied, err = st.Append(ctx, thread, head, records...)
		if err == nil {
			return applied, nil
		}
		if !errors.Is(err, store.ErrRevisionConflict) {
			break
		}
	}
	return 0, fmt.Errorf("agent: append on thread %q: %w", thread, err)
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

// declareTerminalRound durably records a tool-free assistant turn that a
// FinishRound-Continue decision keeps the run alive after: without this
// record a crash before the next round would lose the assistant turn, since
// the natural-done path would have closed the run. The declaration carries
// the terminal_text kind so replay accepts it without tool calls and never
// treats it as an open declaration.
func (p *persistence) declareTerminalRound(ctx context.Context, round int, msg llm.Message) error {
	r := p.roundBase + round
	rec, err := encodeRecord(store.KindRoundDeclared, recordID(p.runID, "-d"+strconv.Itoa(r)),
		roundDeclaredPayload{RunID: p.runID, Round: r, Message: msg, Kind: "terminal_text"})
	if err != nil {
		return err
	}
	if err := p.append(ctx, rec); err != nil {
		return err
	}
	if r > p.lastRound {
		p.lastRound = r
	}
	return nil
}

// closeRun durably closes a FinishRound-End run: the round's declaration
// and commit are already durable, so no message is re-carried. Replay
// derives the terminal view from the log, exactly as it does for the
// natural-done path.
func (p *persistence) closeRun(ctx context.Context, toolCalls int, usage, total llm.Usage) error {
	rec, err := runClosedFor(p.runID, toolCalls, usage, total)
	if err != nil {
		return err
	}
	return p.append(ctx, rec)
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
