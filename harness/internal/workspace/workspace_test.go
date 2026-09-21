package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestRootRejectsEscapesAndSpecialFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("before"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("file", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(dir, "file"), filepath.Join(dir, "hard")); err != nil {
		t.Fatal(err)
	}
	w, err := Open(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for _, path := range []string{"../outside", "/etc/passwd", "link"} {
		t.Run(path, func(t *testing.T) {
			if _, _, err := w.Read(path, 100); err == nil {
				t.Fatal("unsafe read accepted")
			}
		})
	}
	for _, path := range []string{"hard", ".git/config"} {
		t.Run("write "+path, func(t *testing.T) {
			if err := w.Writable(path); err == nil {
				t.Fatal("unsafe write accepted")
			}
		})
	}
	if _, _, err := w.Read("file", 2); err == nil {
		t.Fatal("size limit not enforced")
	}
}

func TestReplaceChecksPreimageAndPreservesMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file")
	if err := os.WriteFile(path, []byte("before"), 0750); err != nil {
		t.Fatal(err)
	}
	w, err := Open(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	change := Change{Path: "file", Before: "stale", After: "after", Exists: true, Mode: 0750}
	if err := w.Replace(context.Background(), change); err == nil {
		t.Fatal("stale preimage accepted")
	}
	change.Before = "before"
	if err := w.Replace(context.Background(), change); err != nil {
		t.Fatal(err)
	}
	data, info, err := w.Read("file", 100)
	if err != nil || string(data) != "after" || info.Mode().Perm() != 0750 {
		t.Fatalf("replacement = %q, %v, %v", data, info, err)
	}
}

func TestPrivateDataAliasCannotBeWritten(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "private"), 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(filepath.Join(dir, "private"), alias); err != nil {
		t.Fatal(err)
	}
	w, err := Open(dir, filepath.Join(alias, "future"))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err := os.Mkdir(filepath.Join(dir, "private/future"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := w.Writable("private/future/new"); err == nil {
		t.Fatal("aliased harness data directory is writable")
	}
}
