package snapshot

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/dailz1/go-agent/harness/internal/workspace"
)

func fixture(t *testing.T, quota int64) (*Store, *workspace.Workspace, string) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(t.TempDir(), "snapshots")
	w, err := workspace.Open(root, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	s, err := Open(dir, w, "thread", quota)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, w, root
}

func credential(w *workspace.Workspace, seq uint64) Credential {
	return Credential{Workspace: w.Path(), ThreadID: "thread", Generation: 1, ToolSequence: seq}
}

func disk(t *testing.T, root, path, text string, mode fs.FileMode) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, path), []byte(text), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, path), mode); err != nil {
		t.Fatal(err)
	}
}

func image(t *testing.T, root, path, text string, mode fs.FileMode) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, path))
	if err != nil || string(b) != text {
		t.Fatalf("disk image = %q, %v; want %q", b, err, text)
	}
	info, err := os.Stat(filepath.Join(root, path))
	if err != nil || info.Mode().Perm() != mode {
		t.Fatalf("disk mode = %v, %v; want %v", info, err, mode)
	}
}

func commit(t *testing.T, s *Store, w *workspace.Workspace, seq uint64, c workspace.Change) Record {
	t.Helper()
	r, err := s.Commit(t.Context(), credential(w, seq), c, func() error { return w.Replace(t.Context(), c) })
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestCommitPreparesBeforeOneApply(t *testing.T) {
	s, w, root := fixture(t, 0)
	disk(t, root, "a", "user dirty\n", 0751)
	c := workspace.Change{Path: "a", Before: "user dirty\n", After: "assistant\n", Exists: true, Mode: 0751}
	calls := 0
	r, err := s.Commit(t.Context(), credential(w, 1), c, func() error {
		calls++
		other, err := Open(s.dir, w, "thread", 0)
		if err != nil {
			return err
		}
		defer other.Close()
		records, err := other.List(t.Context())
		if err != nil || len(records) != 1 || records[0].State != Prepared || records[0].Change != c {
			t.Fatalf("durable preparation missing: %#v, %v", records, err)
		}
		return w.Replace(t.Context(), c)
	})
	if err != nil || r.State != Applied || calls != 1 {
		t.Fatalf("commit = %#v, %v, calls %d", r, err, calls)
	}
	image(t, root, "a", "assistant\n", 0751)
	if _, err := s.Commit(t.Context(), credential(w, 1), c, func() error { calls++; return nil }); !errors.Is(err, ErrCredential) {
		t.Fatalf("reused credential = %v", err)
	}
	if calls != 1 {
		t.Fatal("credential replay invoked apply")
	}
}

func TestRestoreReverseOrderAndIdempotence(t *testing.T) {
	s, w, root := fixture(t, 0)
	disk(t, root, "a", "dirty", 0750)
	first := commit(t, s, w, 1, workspace.Change{Path: "a", Before: "dirty", After: "one", Exists: true, Mode: 0750})
	second := commit(t, s, w, 2, workspace.Change{Path: "./a", Before: "one", After: "two", Exists: true, Mode: 0750})
	if _, err := s.Restore(t.Context(), first.ID); !errors.Is(err, ErrNotLatest) {
		t.Fatalf("old restore = %v", err)
	}
	if r, err := s.Restore(t.Context(), second.ID); err != nil || r.State != Restored {
		t.Fatalf("latest restore = %#v, %v", r, err)
	}
	image(t, root, "a", "one", 0750)
	if _, err := s.Restore(t.Context(), first.ID); err != nil {
		t.Fatal(err)
	}
	image(t, root, "a", "dirty", 0750)
	disk(t, root, "a", "later external", 0640)
	if _, err := s.Restore(t.Context(), second.ID); err != nil {
		t.Fatal(err)
	}
	image(t, root, "a", "later external", 0640)
}

func TestRestoreNewFileAndConflicts(t *testing.T) {
	for _, kind := range []string{"contents", "mode", "missing", "clean"} {
		t.Run(kind, func(t *testing.T) {
			s, w, root := fixture(t, 0)
			if err := os.Mkdir(filepath.Join(root, "sub"), 0755); err != nil {
				t.Fatal(err)
			}
			r := commit(t, s, w, 1, workspace.Change{Path: "sub/new", After: "created"})
			switch kind {
			case "contents":
				disk(t, root, "sub/new", "external", 0644)
			case "mode":
				if err := os.Chmod(filepath.Join(root, "sub/new"), 0600); err != nil {
					t.Fatal(err)
				}
			case "missing":
				if err := os.Remove(filepath.Join(root, "sub/new")); err != nil {
					t.Fatal(err)
				}
			}
			_, err := s.Restore(t.Context(), r.ID)
			if kind != "clean" {
				if !errors.Is(err, ErrConflict) {
					t.Fatalf("restore = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(root, "sub/new")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("new file remains: %v", err)
			}
			if _, err := os.Stat(filepath.Join(root, "sub")); err != nil {
				t.Fatal("parent directory removed")
			}
		})
	}
}

func TestNoopQuotaCancellationAndBinding(t *testing.T) {
	s, w, root := fixture(t, 5)
	disk(t, root, "a", "abc", 0600)
	c := workspace.Change{Path: "a", Before: "abc", After: "abc", Exists: true, Mode: 0600}
	calls := 0
	apply := func() error { calls++; return nil }
	r, err := s.Commit(t.Context(), credential(w, 1), c, apply)
	if err != nil || r.ID != "" || calls != 0 {
		t.Fatalf("no-op = %#v, %v, %d calls", r, err, calls)
	}
	c.After = "def"
	if _, err := s.Commit(t.Context(), credential(w, 2), c, apply); !errors.Is(err, ErrQuota) {
		t.Fatalf("quota = %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.Commit(ctx, credential(w, 3), c, apply); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel = %v", err)
	}
	for _, bad := range []Credential{
		{Workspace: w.Path(), ThreadID: "other", Generation: 1, ToolSequence: 1},
		{Workspace: root + "-other", ThreadID: "thread", Generation: 1, ToolSequence: 1},
		{Workspace: w.Path(), ThreadID: "thread", ToolSequence: 1},
		{Workspace: w.Path(), ThreadID: "thread", Generation: 1},
	} {
		if _, err := s.Commit(t.Context(), bad, c, apply); !errors.Is(err, ErrCredential) {
			t.Fatalf("bad credential = %v", err)
		}
	}
	if calls != 0 {
		t.Fatal("rejected operation invoked apply")
	}
}

func TestPrivateDurableFilesAndReopenBinding(t *testing.T) {
	s, w, _ := fixture(t, 0)
	commit(t, s, w, 1, workspace.Change{Path: "a", After: "secret"})
	err := filepath.WalkDir(s.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		want := fs.FileMode(0600)
		if d.IsDir() {
			want = 0700
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s mode = %v, want %v", path, info.Mode().Perm(), want)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if wrong, err := Open(s.dir, w, "other", 0); !errors.Is(err, ErrCredential) {
		if wrong != nil {
			wrong.Close()
		}
		t.Fatalf("wrong thread opened: %v", err)
	}
}
