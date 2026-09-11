package agent

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"sync"
	"sync/atomic"

	"github.com/dailz1/go-agent/pkg/store"
)

// Persistence errors. They wrap store failures via %w; compare with
// errors.Is. ErrThreadBusy is the nonblocking same-thread conflict: the
// second concurrent run is rejected before it reads the thread or calls the
// provider — optimistic append revisions alone would only collide much
// later, after both model calls were already made. Ownership is global per
// (store, thread), not per Agent: two agents sharing one store must not
// race the same thread into the provider.
var (
	ErrNoStore         = errors.New("agent: no store configured")
	ErrInvalidThreadID = errors.New("agent: invalid thread id")
	ErrThreadBusy      = errors.New("agent: thread has an active run")
	ErrRunIncomplete   = errors.New("agent: thread has an incomplete run; resume it with ResumeThread")
	ErrNothingToResume = errors.New("agent: thread has no incomplete run to resume")
	// ErrStoreNotComparable guards the global ownership registry: Store
	// values are used as map keys, so a value whose dynamic type is not
	// comparable (e.g. a struct with map fields) cannot be registered. Use a
	// pointer-receiver store — both built-in backends qualify.
	ErrStoreNotComparable = errors.New("agent: store value must be comparable for thread ownership")
)

// reservationConflictError marks a kernel-generated thread ID losing the
// rev=0 reservation: the generated ID already names a thread. Run retries
// with a fresh ID and nothing else — a conflict after reservation means real
// concurrent writes, and the run must stop instead of restarting.
type reservationConflictError struct {
	ThreadID string
	Err      error
}

func (e *reservationConflictError) Error() string {
	return fmt.Sprintf("agent: generated thread id %q already exists: %v", e.ThreadID, e.Err)
}

func (e *reservationConflictError) Unwrap() error { return e.Err }

func isReservationConflict(err error) bool {
	var rce *reservationConflictError
	return errors.As(err, &rce)
}

// validateThreadID enforces the ID boundary: nonempty, and short enough that
// the JSONL backend's PathEscape of the ID still fits a file name component
// (255 bytes minus the ".jsonl" suffix). IDs are never normalized.
func validateThreadID(id string) error {
	if id == "" {
		return fmt.Errorf("%w: must be nonempty", ErrInvalidThreadID)
	}
	if escaped := len(url.PathEscape(id)) + len(".jsonl"); escaped > 255 {
		return fmt.Errorf("%w: escapes to a %d-byte file name component, limit 255", ErrInvalidThreadID, escaped)
	}
	return nil
}

// newThreadID returns a crypto-random 128-bit thread ID for kernel-generated
// threads. newRunID is the per-run equivalent used in record IDs. The vars
// are seams: tests stub them for deterministic collision coverage.
var (
	newThreadID = func() (string, error) { return randomID("t") }
	newRunID    = func() (string, error) { return randomID("r") }
)

func randomID(prefix string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("agent: generate id: %w", err)
	}
	return prefix + "-" + hex.EncodeToString(b[:]), nil
}

// threadOwners registers the active run per (store, thread) across every
// Agent instance in the process. Entries are never deleted: removal has an
// ABA race (a waiter holding a detached boolean can win a CAS against a
// newcomer). The accepted cost is one fixed-size entry per thread ever run
// — process-lifetime growth with thread cardinality, not a bounded map.
type ownerKey struct {
	store  store.Store
	thread string
}

var threadOwners sync.Map

// acquireThread takes nonblocking ownership of a thread for one run. A
// non-comparable store value is rejected with ErrStoreNotComparable instead
// of panicking inside the registry.
func acquireThread(st store.Store, thread string) error {
	if !reflect.TypeOf(st).Comparable() {
		return fmt.Errorf("%w; use a pointer-receiver store implementation", ErrStoreNotComparable)
	}
	b, _ := threadOwners.LoadOrStore(ownerKey{st, thread}, &atomic.Bool{})
	if !b.(*atomic.Bool).CompareAndSwap(false, true) {
		return fmt.Errorf("%w: %q", ErrThreadBusy, thread)
	}
	return nil
}

func releaseThread(st store.Store, thread string) {
	if !reflect.TypeOf(st).Comparable() {
		return // never registered
	}
	if b, ok := threadOwners.Load(ownerKey{st, thread}); ok {
		b.(*atomic.Bool).Store(false)
	}
}
