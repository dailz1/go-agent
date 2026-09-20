package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestJSONLSecondLiveInstanceOnSameDirectoryIsRejected(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, err := NewJSONL(dir)
	if err != nil {
		t.Fatalf("NewJSONL: %v", err)
	}
	if _, err := NewJSONL(dir); err == nil {
		t.Error("second live instance on the same directory was allowed")
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := NewJSONL(dir)
	if err != nil {
		t.Fatalf("reopen after Close: %v", err)
	}
	reopened.Close()
}

func TestJSONLRepeatedCloseNeverUnregistersNewerStore(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a, err := NewJSONL(dir)
	if err != nil {
		t.Fatalf("NewJSONL a: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("close a: %v", err)
	}
	b, err := NewJSONL(dir)
	if err != nil {
		t.Fatalf("NewJSONL b: %v", err)
	}
	defer b.Close()
	// The stale instance's repeated Close must not unregister b.
	if err := a.Close(); err != nil {
		t.Fatalf("repeated close a: %v", err)
	}
	if _, err := NewJSONL(dir); err == nil {
		t.Error("third instance allowed while b is live — repeated Close unregistered b")
	}
}

func TestJSONLSymlinkAliasIsCanonicalized(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	target := filepath.Join(base, "real")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	s, err := NewJSONL(target)
	if err != nil {
		t.Fatalf("NewJSONL target: %v", err)
	}
	defer s.Close()
	if _, err := NewJSONL(link); err == nil {
		t.Error("symlink alias allowed a second live instance on the same directory")
	}
}

func TestJSONLUseAfterCloseFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newTestJSONL(t)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("repeated Close = %v, want nil", err)
	}
	if _, err := s.Append(ctx, "t", 0, rec("a", KindRunStarted)); !errors.Is(err, ErrClosed) {
		t.Errorf("Append after Close = %v, want ErrClosed", err)
	}
	if _, err := s.Latest(ctx, "t"); !errors.Is(err, ErrClosed) {
		t.Errorf("Latest after Close = %v, want ErrClosed", err)
	}
	if err := s.Delete(ctx, "t"); !errors.Is(err, ErrClosed) {
		t.Errorf("Delete after Close = %v, want ErrClosed", err)
	}
}

func TestJSONLFreshInstanceDeleteRemovesPersistedThread(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dir := newTestJSONL(t)
	if _, err := s.Append(ctx, "t", 0, rec("a", KindRunStarted)); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// A fresh instance has never loaded the thread; Delete must still remove
	// the persisted file.
	fresh, err := NewJSONL(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer fresh.Close()
	if err := fresh.Delete(ctx, "t"); err != nil {
		t.Fatalf("fresh-instance Delete: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("directory not empty after fresh-instance delete: %d entries", len(entries))
	}
	if _, err := fresh.Append(ctx, "t", 0, rec("b", KindRunStarted)); err != nil {
		t.Errorf("recreate after delete: %v", err)
	}
}
