package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func newTestJSONL(t *testing.T) (*JSONLStore, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := NewJSONL(dir)
	if err != nil {
		t.Fatalf("NewJSONL: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, dir
}

func TestJSONLStoreContract(t *testing.T) {
	t.Parallel()
	exerciseContract(t, func() Store {
		s, _ := newTestJSONL(t)
		return s
	})
	exerciseRetryContract(t, func() Store {
		s, _ := newTestJSONL(t)
		return s
	})
}

func TestJSONLPersistsAcrossReopen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dir := newTestJSONL(t)
	if _, err := s.Append(ctx, "t", 0, rec("a", KindRunStarted), rec("b", KindAgentEvent)); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := NewJSONL(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	state, err := reopened.Latest(ctx, "t")
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if state.Head != 2 || len(state.Tail) != 2 || state.Tail[1].ID != "b" {
		t.Errorf("reopened state = head %d, %d records; want head 2, 2 records ending b", state.Head, len(state.Tail))
	}
}

func TestJSONLTornTailRepairedOnReopen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dir := newTestJSONL(t)
	if _, err := s.Append(ctx, "t", 0, rec("a", KindRunStarted), rec("b", KindAgentEvent)); err != nil {
		t.Fatalf("append: %v", err)
	}
	// Simulate a crash mid-write: partial record bytes with no newline.
	f, err := os.OpenFile(filepath.Join(dir, "t.jsonl"), os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := f.WriteString(`{"seq":2,"kind":"agent_event","id":"c"`); err != nil {
		t.Fatalf("torn write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("torn close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := NewJSONL(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	state, err := reopened.Latest(ctx, "t")
	if err != nil {
		t.Fatalf("Latest after torn tail: %v", err)
	}
	if state.Head != 2 || len(state.Tail) != 2 {
		t.Fatalf("repaired state = head %d, %d records; want head 2, 2 records", state.Head, len(state.Tail))
	}
	if head, err := reopened.Append(ctx, "t", 2, rec("c", KindAgentEvent)); err != nil || head != 3 {
		t.Errorf("append after repair: head %d, err %v; want head 3, nil", head, err)
	}
}
func TestJSONLAppendAfterRepairKeepsByteLayoutStable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dir := newTestJSONL(t)
	if _, err := s.Append(ctx, "t", 0, rec("a", KindRunStarted)); err != nil {
		t.Fatalf("append: %v", err)
	}
	// Crash mid-write of the second record.
	path := filepath.Join(dir, "t.jsonl")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := f.WriteString(`{"seq":1,`); err != nil {
		t.Fatalf("torn write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("torn close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// First reopen repairs the torn tail; retrying the batch completes it.
	reopened, err := NewJSONL(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, err := reopened.Append(ctx, "t", 0,
		rec("a", KindRunStarted), rec("b", KindAgentEvent)); err != nil {
		t.Fatalf("batch retry after repair: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Second reopen: byte layout must be stable — no duplicate or torn lines.
	again, err := NewJSONL(dir)
	if err != nil {
		t.Fatalf("second reopen: %v", err)
	}
	defer again.Close()
	state, err := again.Latest(ctx, "t")
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if state.Head != 2 || len(state.Tail) != 2 || state.Tail[1].ID != "b" {
		t.Fatalf("state = head %d, %d records; want head 2, 2 records ending b", state.Head, len(state.Tail))
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("byte layout changed across reopen:\nbefore: %q\nafter:  %q", before, after)
	}
}
