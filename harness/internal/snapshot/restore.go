package snapshot

import (
	"context"
	"fmt"
)

// Restore accepts only an opaque change ID; callers cannot supply replacement
// images or override the stored workspace/thread credential. Confirmation and
// the exclusive controller operation slot belong to the host.
func (s *Store) Restore(ctx context.Context, id string) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(ctx); err != nil {
		return Record{}, err
	}
	index := -1
	for i, r := range s.records {
		if r.ID == id {
			index = i
			break
		}
	}
	if index < 0 {
		return Record{}, ErrNotFound
	}
	r := s.records[index]
	if !s.validCredential(r.Credential) {
		return r, ErrCredential
	}
	if r.State == Restored || r.State == Unapplied {
		return r, nil
	}
	for _, newer := range s.records[index+1:] {
		if newer.Change.Path == r.Change.Path && newer.State != Restored && newer.State != Unapplied {
			return r, ErrNotLatest
		}
	}
	before := s.w.Verify(r.Change) == nil
	afterErr := s.w.Verify(afterImage(r.Change))
	if r.State == Prepared && before {
		r.State = Unapplied
		return s.finishRestore(index, r)
	}
	if r.State == RestorePrepared && before {
		// A rename/removal might have landed without its directory sync.
		if err := s.w.SyncParent(ctx, r.Change.Path); err != nil {
			return r, err
		}
		r.State = Restored
		return s.finishRestore(index, r)
	}
	if afterErr != nil {
		return r, fmt.Errorf("%w: %w", ErrConflict, afterErr)
	}
	if r.State != RestorePrepared {
		next := r
		next.State = RestorePrepared
		if err := s.save(next); err != nil {
			return r, err
		}
		r = next
		s.records[index] = r
	}
	if err := s.w.Restore(ctx, r.Change); err != nil {
		return r, err
	}
	r.State = Restored
	return s.finishRestore(index, r)
}

func (s *Store) finishRestore(index int, r Record) (Record, error) {
	if err := s.save(r); err != nil {
		return s.records[index], err
	}
	s.records[index] = r
	return r, nil
}
