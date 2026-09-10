package store

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"
)

// TestJSONLCloseWaitsForInFlightOperation pins the lifecycle invariant that
// makes the drain sound: while an operation holds the thread lock, Close is
// draining — the directory stays registered (no replacement store can be
// created) — and Close does not return until the operation finishes.
func TestJSONLCloseWaitsForInFlightOperation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dir := newTestJSONL(t)
	if _, err := s.Append(ctx, "t", 0, rec("a", KindRunStarted)); err != nil {
		t.Fatalf("append: %v", err)
	}

	// Re-enter the thread exactly as an in-flight operation would: holding
	// its lock for the duration of the work.
	th, err := s.lockThread(ctx, "t")
	if err != nil {
		t.Fatalf("lockThread: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Close() }()

	// Bounded wait: whether or not the close goroutine has reached its
	// drain, the directory must stay registered while the thread lock is
	// held by the in-flight operation.
	select {
	case err := <-done:
		t.Fatalf("Close returned (%v) while the in-flight operation held the thread lock", err)
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := NewJSONL(dir); err == nil {
		th.mu.Unlock()
		t.Fatal("replacement store created while the directory was still registered")
	}

	th.mu.Unlock()
	if err := <-done; err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Only after the drain may the directory change hands.
	reopened, err := NewJSONL(dir)
	if err != nil {
		t.Errorf("reopen after drain: %v", err)
	} else {
		reopened.Close()
	}
}

// TestJSONLConcurrentCloseCallsWaitForDrain is the deterministic regression
// for concurrent Close: while an in-flight operation holds the thread lock,
// neither of two concurrent Close calls may return (a buggy early return
// would report success while the directory is still registered), and both
// must observe the same drain result.
func TestJSONLConcurrentCloseCallsWaitForDrain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dir := newTestJSONL(t)
	if _, err := s.Append(ctx, "t", 0, rec("a", KindRunStarted)); err != nil {
		t.Fatalf("append: %v", err)
	}

	th, err := s.lockThread(ctx, "t")
	if err != nil {
		t.Fatalf("lockThread: %v", err)
	}
	errA := make(chan error, 1)
	errB := make(chan error, 1)
	go func() { errA <- s.Close() }()
	go func() { errB <- s.Close() }()

	// Bounded wait: a buggy early-returning Close completes here even though
	// the in-flight operation holds the thread lock; the correct
	// implementation keeps draining until the lock is released.
	aDone, bDone := false, false
	select {
	case err := <-errA:
		aDone = true
		t.Errorf("Close A returned (%v) before the in-flight operation finished", err)
	case err := <-errB:
		bDone = true
		t.Errorf("Close B returned (%v) before the in-flight operation finished", err)
	case <-time.After(100 * time.Millisecond):
	}

	if _, err := NewJSONL(dir); err == nil {
		th.mu.Unlock()
		t.Fatal("replacement store created while a drain was in flight")
	}

	th.mu.Unlock()
	// Join only the closes that have not returned yet: on a buggy
	// implementation one of them already returned inside the bounded wait,
	// and waiting on it again would hang instead of failing.
	if !aDone {
		if err := <-errA; err != nil {
			t.Errorf("Close A = %v, want nil (joins the same drain)", err)
		}
	}
	if !bDone {
		if err := <-errB; err != nil {
			t.Errorf("Close B = %v, want nil (joins the same drain)", err)
		}
	}
	reopened, err := NewJSONL(dir)
	if err != nil {
		t.Errorf("reopen after drain: %v", err)
	} else {
		reopened.Close()
	}
}

// TestJSONLCloseDrainsConcurrentAppends closes concurrently with admitted
// appends: every append either completes or fails with ErrClosed, every
// completed append is durably persisted, and Close fully joins the drain
// before the directory is reopened.
func TestJSONLCloseDrainsConcurrentAppends(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, err := NewJSONL(dir)
	if err != nil {
		t.Fatalf("NewJSONL: %v", err)
	}
	ctx := context.Background()
	const n = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	succeeded := map[string]bool{}
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := threadName(i)
			_, err := s.Append(ctx, name, 0, rec("a", KindRunStarted))
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				succeeded[name] = true
			} else if !errors.Is(err, ErrClosed) {
				t.Errorf("append %s: %v; want success or ErrClosed", name, err)
			}
		}(i)
	}
	closeDone := make(chan struct{})
	go func() {
		s.Close()
		close(closeDone)
	}()
	wg.Wait()
	<-closeDone // join the close goroutine: the drain is complete
	// A defensive repeated Close returns immediately, joining the same drain.
	if err := s.Close(); err != nil {
		t.Fatalf("repeated Close: %v", err)
	}

	reopened, err := NewJSONL(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	for name := range succeeded {
		state, err := reopened.Latest(ctx, name)
		if err != nil || state.Head != 1 {
			t.Errorf("succeeded thread %s: head %d, err %v; want head 1, nil", name, state.Head, err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != len(succeeded) {
		t.Errorf("file count = %d, want %d (one per succeeded thread)", len(entries), len(succeeded))
	}
}
