// Package store provides durable session persistence for agent threads.
//
// A thread is a canonical record log: lifecycle records (run_started with
// the run input and thread system prompt, round_declared with a round's full
// assistant message, round_committed with its ordered model-visible results,
// error) and checkpoint records alternate with versioned agent-event
// envelopes. The log
// is the source of truth; checkpoints are rebuildable accelerators (a history
// snapshot positioned at a point in the log) and are safe to discard.
//
// Concurrency contract (v1): single process. The package is safe for
// concurrent use; writes to one thread are serialized. Cross-process writers
// are unsupported, and a JSONL directory may have exactly one live store
// instance (enforced by NewJSONL). Thread IDs and their discovery/indexing
// are caller-owned: the Store never enumerates threads.
package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/dailz1/go-agent/llm"
)

// Envelope schema version of the records written by this package. Bump on
// incompatible Record or payload changes; readers reject newer versions
// with ErrUnsupportedSchema.
const (
	SchemaV1 = 1
	SchemaV2 = 2
)

// Record kinds. Append rejects unknown kinds so a stale binary cannot write
// records it does not understand; reading tolerates unknown kinds and passes
// them through untouched, because rejecting them is a codec policy (the
// agent layer decides which kinds affect state).
const (
	KindRunStarted     = "run_started"     // payload: run input and thread system prompt
	KindRunCancelled   = "run_cancelled"   // payload: run cancellation and unknown round results
	KindAgentEvent     = "agent_event"     // payload: versioned AgentEvent envelope
	KindRoundDeclared  = "round_declared"  // payload: the round's full assistant message
	KindRoundCommitted = "round_committed" // payload: the round's ordered model-visible results
	KindError          = "error"           // payload: terminal stream error description
	KindCheckpoint     = "checkpoint"      // payload: Checkpoint
)

// Errors returned by Store implementations. Wrapped I/O failures remain
// discoverable via errors.Is/As on the wrapped error.
var (
	// ErrRevisionConflict is returned when the thread's head state does not
	// match the expected revision of an Append in a way that a retry can
	// reconcile. Re-read Latest and rebuild the batch.
	ErrRevisionConflict = errors.New("store: thread head revision conflict")
	// ErrUnsupportedSchema is returned for records whose envelope schema
	// version is newer than this package understands.
	ErrUnsupportedSchema = errors.New("store: unsupported record schema")
	// ErrCorruptLog is returned when a durable log fails structural checks:
	// interior corruption, sequence gaps, or structurally invalid records. A
	// torn final line is not corruption — the JSONL backend repairs it by
	// truncation on load.
	ErrCorruptLog = errors.New("store: corrupt thread log")
	// ErrUnknownKind is returned by Append for record kinds this package does
	// not know.
	ErrUnknownKind = errors.New("store: unknown record kind")
	// ErrInvalidRecord is returned for records that violate structural rules
	// (empty ID, missing required payload, malformed payload JSON, duplicate
	// or already-used record ID).
	ErrInvalidRecord = errors.New("store: invalid record")
)

// Record is one durable entry in a thread's log. Seq and RecordedAt are
// assigned by the Store on Append; ID, Kind, Schema, and Payload are the
// caller's immutable content: an ID permanently identifies exactly one
// record, and a retry must reproduce identical content.
type Record struct {
	Seq        int64           `json:"seq"`
	Kind       string          `json:"kind"`
	Schema     int             `json:"schema"`
	ID         string          `json:"id"`
	RecordedAt time.Time       `json:"recorded_at"`
	Payload    json.RawMessage `json:"payload,omitempty"`
}

// Checkpoint is a rebuildable accelerator: a history snapshot positioned at
// the checkpoint record's own log position. Recovery replays only records
// after the checkpoint record.
type Checkpoint struct {
	History []llm.Message `json:"history"`
}

// ThreadState is the durable state of a thread returned by Latest.
type ThreadState struct {
	// Checkpoint is the newest checkpoint record, or nil when the thread has
	// none. Its Seq marks the replay boundary: Tail holds Seq > Checkpoint.Seq.
	Checkpoint *Record
	// Tail holds the records after the newest checkpoint (or all records when
	// no checkpoint exists), in sequence order.
	Tail []Record
	// Head is the next assignable sequence: the revision Append expects.
	Head int64
}

// Store is the durable session persistence contract. Threads are created
// implicitly by the first Append (expected = 0); there is no explicit create.
type Store interface {
	// Append atomically appends records to the thread when the thread's head
	// equals expected, assigning Seq sequentially from expected. It returns
	// the thread's head afterwards: expected+len(records) for a fresh append,
	// or the current head when the batch was already (partially) applied.
	//
	// An empty batch only validates the revision: it returns the current
	// head when expected does not exceed it.
	//
	// The operation is idempotent for ambiguous-failure retries. Let k be the
	// number of batch records already present at [expected, expected+k): the
	// prefix must match the batch by full caller content (ID, Kind, Schema,
	// Payload), and any remaining records must use IDs not present in the
	// log. Everything else is ErrRevisionConflict.
	Append(ctx context.Context, thread string, expected int64, records ...Record) (int64, error)

	// Latest returns the thread's durable state. Unknown or future record
	// kinds appear in Tail untouched.
	Latest(ctx context.Context, thread string) (ThreadState, error)

	// History returns the thread's records with Seq >= from, in sequence
	// order, including records a checkpoint would hide: unlike Latest it
	// never omits a prefix. It is the fallback read for replay when the
	// newest checkpoint cannot be trusted; from <= 0 selects the entire
	// log (records start at Seq 0). Unknown threads yield an empty result.
	// The returned records are copies; mutating them does not affect the
	// store.
	History(ctx context.Context, thread string, from int64) ([]Record, error)

	// Delete removes the thread and all its records. Deleting an unknown
	// thread — including one that exists on disk but has not been loaded in
	// this process — is equivalent to deleting an empty thread. The thread
	// may be recreated by a later Append.
	Delete(ctx context.Context, thread string) error
}

// prepareBatch ingests caller-owned records: it deep-copies payloads, normal
// izes an unset schema, and applies structural validation. Both backends must
// run it under their locks before any classification, so stored state never
// aliases caller memory and validation cannot diverge.
func prepareBatch(batch []Record) ([]Record, error) {
	seen := make(map[string]struct{}, len(batch))
	out := make([]Record, len(batch))
	for i := range batch {
		r := batch[i]
		if r.Schema == 0 {
			r.Schema = SchemaV1
		}
		if err := validateRecord(&r); err != nil {
			return nil, err
		}
		if _, dup := seen[r.ID]; dup {
			return nil, ErrInvalidRecord
		}
		seen[r.ID] = struct{}{}
		r.Payload = append(json.RawMessage(nil), r.Payload...)
		out[i] = r
	}
	return out, nil
}

// planAppend classifies an Append against the current log and returns the
// records that still need to be applied plus the thread's resulting head.
// It implements the documented retry semantics: an applied prefix (matching
// by full caller content) is accepted, the remainder must use fresh IDs, and
// anything else is a conflict. Both backends share it so retry semantics
// cannot diverge. The batch must have passed prepareBatch.
func planAppend(log []Record, expected int64, batch []Record) (apply []Record, head int64, err error) {
	head = int64(len(log))
	if expected < 0 || expected > head {
		return nil, 0, ErrRevisionConflict
	}
	k := int(head - expected)
	if k > len(batch) {
		k = len(batch)
	}
	for i := 0; i < k; i++ {
		applied := log[int(expected)+i]
		want := batch[i]
		if applied.ID != want.ID || applied.Kind != want.Kind ||
			applied.Schema != want.Schema || !bytes.Equal(applied.Payload, want.Payload) {
			return nil, 0, ErrRevisionConflict
		}
	}
	if len(batch) > k {
		known := make(map[string]struct{}, len(log))
		for i := range log {
			known[log[i].ID] = struct{}{}
		}
		for _, r := range batch[k:] {
			if _, taken := known[r.ID]; taken {
				return nil, 0, ErrInvalidRecord
			}
		}
	}
	apply = batch[k:]
	head = expected + int64(len(batch))
	if head < int64(len(log)) {
		head = int64(len(log))
	}
	return apply, head, nil
}

func validateRecord(r *Record) error {
	switch r.Kind {
	case KindRunStarted, KindRunCancelled, KindAgentEvent, KindRoundDeclared, KindRoundCommitted, KindError, KindCheckpoint:
	default:
		return ErrUnknownKind
	}
	if r.ID == "" {
		return ErrInvalidRecord
	}
	if r.Schema < 0 || r.Schema > SchemaV2 {
		return ErrUnsupportedSchema
	}
	if r.Kind == KindRunCancelled && r.Schema != SchemaV2 {
		return ErrUnsupportedSchema
	}
	if len(r.Payload) == 0 {
		if r.Kind == KindCheckpoint {
			return ErrInvalidRecord
		}
		return nil
	}
	if !json.Valid(r.Payload) {
		return ErrInvalidRecord
	}
	return nil
}

// historyStart maps a History from bound (Seq >= from) to a slice index over
// n records numbered from Seq 0: from <= 0 selects the whole log; an
// overshooting from selects the empty tail.
func historyStart(from, n int64) int {
	switch {
	case from <= 0:
		return 0
	case from >= n:
		return int(n)
	default:
		return int(from)
	}
}

// timeNow is a seam for tests to stub the clock.
var timeNow = time.Now
