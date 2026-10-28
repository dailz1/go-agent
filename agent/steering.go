package agent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
)

// MaxEnqueueInputRunes is the absolute upper bound for one steering input.
// The effective admission bound is narrower: an Agent admits an input only
// if it fits the compaction headroom of the configured context window, so
// an accepted input can never push an active run past its budget at the
// drain point.
const MaxEnqueueInputRunes = 8192

// QueuedInputID identifies one enqueued steering input. It is derived from
// the caller-supplied idempotency key: the same key names the same logical
// message, so an ambiguous-failure retry confirms instead of duplicating.
type QueuedInputID string

// ErrEmptyInput rejects an enqueue call with an empty input or key.
var ErrEmptyInput = errors.New("agent: steering input and idempotency key must be non-empty")

// ErrInputTooLarge rejects an enqueue call whose input exceeds
// MaxEnqueueInputRunes.
var ErrInputTooLarge = errors.New("agent: steering input exceeds the maximum length")

// queueEntry is one pending steering input: the record ID of the
// input_queued record it came from plus the raw input text.
type queueEntry struct {
	ID    string `json:"id"`
	Input string `json:"input"`
}

// inputQueuedPayload is the intent record written by EnqueueInputWithID.
// It changes no history; replay keeps the input queued until a matching
// input_consumed record drains it.
type inputQueuedPayload struct {
	Input string `json:"input"`
}

// consumedInput pairs one drained queue entry with the queued record it
// came from, so replay can validate the FIFO prefix instead of trusting
// raw text.
type consumedInput struct {
	ID    string `json:"id"`
	Input string `json:"input"`
}

// inputConsumedPayload is written by the runner at the D1 drain point: the
// ordered FIFO prefix of the queue that is being injected into history as
// role=user messages.
type inputConsumedPayload struct {
	Inputs []consumedInput `json:"inputs"`
}

// EnqueueInputWithID durably queues a steering input for a thread. The
// record lands immediately — before any run consumes it — so a crash never
// loses an input the API already accepted. The change reaches the model at
// the next drain point: the next round boundary of an active run, or the
// first round of the next run on the thread.
//
// The idempotency key names the logical message: retrying this call with
// the same key confirms instead of duplicating, even across restarts and
// checkpoints. A different key with equal text is a second message.
func (a *Agent) EnqueueInputWithID(ctx context.Context, thread, idempotencyKey, input string) (QueuedInputID, error) {
	if a.store == nil {
		return "", ErrNoStore
	}
	if idempotencyKey == "" || input == "" {
		return "", ErrEmptyInput
	}
	// Admission is measured on the serialized message — the same unit the
	// compaction budget and the drain use — so escaping-heavy text cannot
	// slip past the bound on raw rune count alone.
	cost := estimateRunes([]llm.Message{llm.UserMessage(input)})
	if cost > MaxEnqueueInputRunes || cost > a.steeringAdmissionRunes() {
		return "", fmt.Errorf("agent: steering input costs %d serialized runes, admission bound %d: %w",
			cost, a.steeringAdmissionRunes(), ErrInputTooLarge)
	}
	sum := sha256.Sum256([]byte(idempotencyKey))
	id := QueuedInputID("q-" + hex.EncodeToString(sum[:8]))
	rec, err := encodeRecord(store.KindInputQueued, string(id), inputQueuedPayload{Input: input})
	if err != nil {
		return "", fmt.Errorf("agent: enqueue input on thread %q: %w", thread, err)
	}
	rec.Schema = store.SchemaV2

	const maxAttempts = 5
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		state, err := a.store.Latest(ctx, thread)
		if err != nil {
			lastErr = err
			continue
		}
		if queuedRecordLanded(state.Tail, string(id), string(rec.Payload)) {
			return id, nil
		}
		_, err = a.store.Append(ctx, thread, state.Head, rec)
		if err == nil {
			return id, nil
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if errors.Is(err, store.ErrInvalidRecord) {
			// The ID is already taken somewhere the tail scan cannot see
			// (behind a checkpoint). Confirm against the full log before
			// treating it as a landed duplicate of this exact key.
			full, ferr := a.store.History(ctx, thread, 0)
			if ferr != nil {
				lastErr = ferr
				continue
			}
			if queuedRecordLanded(full, string(id), string(rec.Payload)) {
				return id, nil
			}
			lastErr = err
			continue
		}
		lastErr = err
	}
	if errors.Is(lastErr, store.ErrRevisionConflict) {
		return "", fmt.Errorf("agent: enqueue input on thread %q inconclusive after %d attempts (the input may or may not have landed): %w", thread, maxAttempts, ErrThreadBusy)
	}
	return "", fmt.Errorf("agent: enqueue input on thread %q: %w", thread, lastErr)
}

// queuedRecordLanded confirms an ambiguous enqueue by full content.
func queuedRecordLanded(tail []store.Record, id, payload string) bool {
	for _, r := range tail {
		if r.ID == id && r.Kind == store.KindInputQueued &&
			r.Schema == store.SchemaV2 && string(r.Payload) == payload {
			return true
		}
	}
	return false
}

// drainInputs consumes the pending steering queue at a drain point: every
// pending entry that fits availableRunes becomes one role=user message
// appended to history, and one input_consumed record lands atomically
// before the round proceeds. Entries that do not fit stay queued for the
// next drain, after compaction has freed space — an accepted input can
// defer, but never push the run past its hard limit. The returned messages
// must be appended to the live history by the caller.
func (p *persistence) drainInputs(ctx context.Context, availableRunes int) ([]llm.Message, error) {
	if err := p.absorbTail(ctx, p.cfgRead, false); err != nil {
		return nil, err
	}
	if len(p.queue) == 0 {
		return nil, nil
	}
	msgs := make([]llm.Message, 0, len(p.queue))
	entries := make([]consumedInput, 0, len(p.queue))
	used := 0
	n := 0
	for _, e := range p.queue {
		cost := estimateRunes([]llm.Message{llm.UserMessage(e.Input)})
		if used+cost > availableRunes {
			break
		}
		used += cost
		msgs = append(msgs, llm.UserMessage(e.Input))
		entries = append(entries, consumedInput{ID: e.ID, Input: e.Input})
		n++
	}
	if n == 0 {
		return nil, nil
	}
	rec, err := encodeRecord(store.KindInputConsumed, newRecordNonce(), inputConsumedPayload{Inputs: entries})
	if err != nil {
		return nil, err
	}
	rec.Schema = store.SchemaV2
	if err := p.append(ctx, rec); err != nil {
		return nil, err
	}
	// Remove only the consumed prefix: entries absorbed into the queue
	// while this append was rebasing must survive for the next drain.
	p.queue = p.queue[n:]
	p.carry = estimateRunes(msgs)
	return msgs, nil
}

// newRecordNonce mints a fresh random ID for a runner-written control
// record (ambiguous retries are handled by the append absorb protocol).
func newRecordNonce() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("agent: mint record nonce: %v", err))
	}
	return "ctl-" + hex.EncodeToString(b[:])
}
