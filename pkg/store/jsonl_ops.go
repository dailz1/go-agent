package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// Append implements Store.
func (s *JSONLStore) Append(ctx context.Context, thread string, expected int64, records ...Record) (int64, error) {
	if err := s.checkOpen(); err != nil {
		return 0, err
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	th, err := s.lockThread(ctx, thread)
	if err != nil {
		return 0, err
	}
	defer th.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	batch, err := prepareBatch(records)
	if err != nil {
		return 0, err
	}
	apply, head, err := planAppend(th.records, expected, batch)
	if err != nil {
		return 0, err
	}
	if len(apply) == 0 {
		return head, nil // fully applied already; nothing to write
	}

	now := time.Now()
	var buf bytes.Buffer
	stamped := make([]Record, len(apply))
	copy(stamped, apply)
	for i := range stamped {
		stamped[i].Seq = head - int64(len(apply)) + int64(i)
		stamped[i].RecordedAt = now
		line, err := json.Marshal(stamped[i])
		if err != nil {
			return 0, fmt.Errorf("store: encode record: %w", err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if th.file == nil {
		f, err := os.OpenFile(th.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return 0, fmt.Errorf("store: open log: %w", err)
		}
		if err := syncDir(s.dir); err != nil {
			f.Close()
			return 0, err
		}
		th.file = f
	}
	if n, err := th.file.Write(buf.Bytes()); err != nil || n != buf.Len() {
		return 0, s.reconcile(th, fmt.Errorf("store: write log: %w", err))
	}
	if err := th.file.Sync(); err != nil {
		return 0, s.reconcile(th, fmt.Errorf("store: sync log: %w", err))
	}
	th.records = append(th.records, stamped...)
	return head, nil
}

// reconcile re-derives the in-memory view from durable bytes after an
// ambiguous write or sync failure, so retries classify against what is
// actually on disk, and returns the original error.
func (s *JSONLStore) reconcile(th *jsonlThread, cause error) error {
	if err := th.load(); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

// Latest implements Store.
func (s *JSONLStore) Latest(ctx context.Context, thread string) (ThreadState, error) {
	if err := s.checkOpen(); err != nil {
		return ThreadState{}, err
	}
	if err := ctx.Err(); err != nil {
		return ThreadState{}, err
	}
	th, err := s.lockThread(ctx, thread)
	if err != nil {
		return ThreadState{}, err
	}
	defer th.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return ThreadState{}, err
	}

	state := ThreadState{Head: int64(len(th.records))}
	if idx := lastCheckpoint(th.records); idx >= 0 {
		cp := cloneRecord(th.records[idx])
		state.Checkpoint = &cp
		state.Tail = cloneRecords(th.records[idx+1:])
	} else {
		state.Tail = cloneRecords(th.records)
	}
	return state, nil
}

// History implements Store.
func (s *JSONLStore) History(ctx context.Context, thread string, from int64) ([]Record, error) {
	if err := s.checkOpen(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	th, err := s.lockThread(ctx, thread)
	if err != nil {
		return nil, err
	}
	defer th.mu.Unlock()
	return cloneRecords(th.records[historyStart(from, int64(len(th.records))):]), nil
}

// Delete implements Store. It loads (and crash-repairs) the thread first, so
// a persisted-but-unloaded thread is genuinely removed; the operation is
// linearized against Append/Latest under the thread lock.
func (s *JSONLStore) Delete(ctx context.Context, thread string) error {
	if err := s.checkOpen(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	th, err := s.lockThread(ctx, thread)
	if err != nil {
		return err
	}
	defer th.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if th.file != nil {
		if err := th.file.Close(); err != nil {
			return fmt.Errorf("store: close log: %w", err)
		}
		th.file = nil
	}
	_, statErr := os.Stat(th.path)
	switch {
	case statErr == nil:
		if err := os.Remove(th.path); err != nil {
			return fmt.Errorf("store: remove log: %w", err)
		}
		if err := syncDir(s.dir); err != nil {
			return err
		}
	case !os.IsNotExist(statErr):
		return fmt.Errorf("store: stat log: %w", statErr)
	}
	// Keep the same thread object (so concurrent access cannot reload a
	// stale file) and reset it to a fresh, empty thread.
	th.records = nil
	th.loaded = true
	return nil
}

// lockThread returns the thread with its mutex HELD; callers must unlock it
// (typically via defer) when their work is done. Holding the lock across the
// whole operation is what makes Close's drain sound: an operation admitted
// before Close runs to completion before Close can unregister the
// directory, and an operation that lost the admission race is rejected here
// instead of executing after Close returned. The closed recheck happens
// after th.mu is acquired, closing the unlock/relock gap between admission
// and work. The thread object stays in the map for the store's lifetime.
func (s *JSONLStore) lockThread(ctx context.Context, name string) (*jsonlThread, error) {
	s.mu.Lock()
	if s.closed.Load() {
		s.mu.Unlock()
		return nil, ErrClosed
	}
	th := s.threads[name]
	if th == nil {
		th = &jsonlThread{path: filepath.Join(s.dir, url.PathEscape(name)+".jsonl")}
		s.threads[name] = th
	}
	s.mu.Unlock()

	th.mu.Lock()
	if s.closed.Load() {
		// Lost the admission race with Close: never execute after Close
		// could have returned and handed the directory to a replacement.
		th.mu.Unlock()
		return nil, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		th.mu.Unlock()
		return nil, err
	}
	if th.loaded {
		return th, nil
	}
	if err := th.load(); err != nil {
		th.mu.Unlock()
		return nil, err
	}
	th.loaded = true
	return th, nil
}
