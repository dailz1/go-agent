package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/dailz1/go-agent/store"
)

// ErrEmptyConfig rejects a SetThreadConfig call whose every field is nil.
var ErrEmptyConfig = errors.New("agent: config change carries no fields")

// ErrInvalidConfigChange rejects a SetThreadConfig call whose Provider
// field is syntactically invalid: an empty provider name, or a provider
// switch that does not carry a model name (the provider+model pair rule).
var ErrInvalidConfigChange = errors.New("agent: invalid config change")

// ErrUnknownProvider aborts a round whose thread configuration names a
// provider that was not injected at construction (WithProviders). The run
// stops fail-closed instead of silently falling back to the default
// provider; the thread stays resumable, and a later SetThreadConfig with
// a valid name repairs it (last write wins).
var ErrUnknownProvider = errors.New("agent: unknown provider")

// ConfigChange is a runtime config override for one thread. A nil field
// leaves the corresponding value unchanged; a non-nil field overwrites it
// for every subsequent provider request (last write wins across records).
// Provider switches are paired with the model: a non-nil Provider must
// carry a non-empty Model in the same record, and the folded model then
// mechanically shadows any agent-level WithModel option.
type ConfigChange struct {
	Model    *string
	Thinking *string
	Provider *string
}

// SetThreadConfig durably retunes a thread's runtime config. Each call is a
// distinct operation with its own fresh random record ID: repeating a
// previously used value appends a new record (A→B→A must end on A), so the
// ID confirms only this invocation's ambiguous retries, never older equal
// content. It is an external control-plane writer: the record is appended
// optimistically at the current head; a revision conflict rebases at the
// true head. The change takes effect at the next round boundary of an
// active run (and at the first round of the next run); it never interrupts
// the in-flight request.
//
// A Provider switch names the target provider AND its model in one call;
// the two land as one record and take effect atomically at the next round
// boundary of an active run (and at the first round of the next run).
//
// After maxAttempts the error is ErrThreadBusy only when every failure was
// a confirmed revision conflict; any other terminal store failure is
// returned wrapped as-is. An inconclusive conflict means the change may or
// may not have landed — the next call with the same or different values
// remains correct either way.
func SetThreadConfig(ctx context.Context, st store.Store, thread string, ch ConfigChange) error {
	if st == nil {
		return ErrNoStore
	}
	if ch.Model == nil && ch.Thinking == nil && ch.Provider == nil {
		return ErrEmptyConfig
	}
	// Cheap syntax checks the writer can do without an Agent: the provider
	// name must be non-empty and paired with a model. Whether the name is
	// actually injected is only knowable at round-top resolution, which
	// fails closed with ErrUnknownProvider.
	if ch.Provider != nil {
		if *ch.Provider == "" {
			return fmt.Errorf("agent: provider name must not be empty: %w", ErrInvalidConfigChange)
		}
		if ch.Model == nil || *ch.Model == "" {
			return fmt.Errorf("agent: provider switch must carry a model name: %w", ErrInvalidConfigChange)
		}
	}
	payload := configChangedPayload{Model: ch.Model, Thinking: ch.Thinking, Provider: ch.Provider}
	id, err := newControlRecordID()
	if err != nil {
		return fmt.Errorf("agent: set thread config on %q: %w", thread, err)
	}
	rec, err := encodeRecord(store.KindConfigChanged, id, payload)
	if err != nil {
		return err
	}
	rec.Schema = store.SchemaV2

	const maxAttempts = 5
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		state, err := st.Latest(ctx, thread)
		if err != nil {
			lastErr = err
			continue
		}
		if configRecordLanded(state.Tail, rec) {
			return nil
		}
		_, err = st.Append(ctx, thread, state.Head, rec)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		lastErr = err
	}
	if errors.Is(lastErr, store.ErrRevisionConflict) {
		return fmt.Errorf("agent: set thread config on %q inconclusive after %d attempts (the change may or may not have landed): %w", thread, maxAttempts, ErrThreadBusy)
	}
	return fmt.Errorf("agent: set thread config on %q: %w", thread, lastErr)
}

// configRecordLanded confirms an ambiguous attempt by full content: ID,
// kind, schema, and payload must all match the record this invocation sent.
func configRecordLanded(tail []store.Record, rec store.Record) bool {
	for _, r := range tail {
		if r.ID == rec.ID && r.Kind == rec.Kind && r.Schema == rec.Schema &&
			string(r.Payload) == string(rec.Payload) {
			return true
		}
	}
	return false
}

// newControlRecordID mints a fresh random ID for one control-plane
// operation. Operation identity is invocation-scoped, never derived from
// content: repeating a value is a new operation that must append a new
// record.
func newControlRecordID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("agent: mint control record id: %w", err)
	}
	return "cfg-" + hex.EncodeToString(b[:]), nil
}
