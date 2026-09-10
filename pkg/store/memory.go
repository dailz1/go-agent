package store

import (
	"context"
	"encoding/json"
	"sync"
)

// MemoryStore is an in-memory Store. It is process-local and loses all data
// when the process exits; use it for tests and as the zero-configuration
// backend. It is safe for concurrent use; writes are serialized store-wide.
type MemoryStore struct {
	mu      sync.Mutex
	threads map[string][]Record
}

// NewMemory returns an empty in-memory Store.
func NewMemory() *MemoryStore {
	return &MemoryStore{threads: make(map[string][]Record)}
}

// Append implements Store.
func (m *MemoryStore) Append(ctx context.Context, thread string, expected int64, records ...Record) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	batch, err := prepareBatch(records)
	if err != nil {
		return 0, err
	}
	apply, head, err := planAppend(m.threads[thread], expected, batch)
	if err != nil {
		return 0, err
	}
	if len(apply) > 0 {
		stamped := make([]Record, len(apply))
		copy(stamped, apply)
		now := timeNow()
		for i := range stamped {
			stamped[i].Seq = head - int64(len(apply)) + int64(i)
			stamped[i].RecordedAt = now
		}
		m.threads[thread] = append(m.threads[thread], stamped...)
	}
	return head, nil
}

// Latest implements Store.
func (m *MemoryStore) Latest(ctx context.Context, thread string) (ThreadState, error) {
	if err := ctx.Err(); err != nil {
		return ThreadState{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return ThreadState{}, err
	}
	log := m.threads[thread]

	state := ThreadState{Head: int64(len(log))}
	if idx := lastCheckpoint(log); idx >= 0 {
		cp := cloneRecord(log[idx])
		state.Checkpoint = &cp
		state.Tail = cloneRecords(log[idx+1:])
	} else {
		state.Tail = cloneRecords(log)
	}
	return state, nil
}

// Delete implements Store. Deleting an unknown thread is a no-op.
func (m *MemoryStore) Delete(ctx context.Context, thread string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	delete(m.threads, thread)
	return nil
}

// lastCheckpoint returns the index of the final checkpoint record, or -1.
func lastCheckpoint(log []Record) int {
	for i := len(log) - 1; i >= 0; i-- {
		if log[i].Kind == KindCheckpoint {
			return i
		}
	}
	return -1
}

func cloneRecord(r Record) Record {
	out := r
	out.Payload = append(json.RawMessage(nil), r.Payload...)
	return out
}

func cloneRecords(rs []Record) []Record {
	out := make([]Record, len(rs))
	for i, r := range rs {
		out[i] = cloneRecord(r)
	}
	return out
}
