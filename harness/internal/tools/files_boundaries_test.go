package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/dailz1/go-agent/harness/internal/workspace"
)

func TestEditRejectsAmbiguityAndSizeBeforeGate(t *testing.T) {
	tests := []struct {
		name string
		old  string
		new  string
	}{
		{name: "missing", old: "missing", new: "x"},
		{name: "overlapping", old: "aa", new: "x"},
		{name: "empty", old: "", new: "x"},
		{name: "binary", old: "aaa", new: "\x00"},
		{name: "oversize", old: "aaa", new: strings.Repeat("x", workspace.MaxFileBytes+1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gate := &applyGate{}
			f := setup(t, gate)
			put(t, f, "a", "aaa")
			read := decode[ReadObservation](t, call(t, f, "read", map[string]any{"path": "a"}))
			result := call(t, f, "edit", map[string]any{
				"path": "a", "expected_hash": read.Hash, "old_text": tt.old, "new_text": tt.new,
			})
			if !result.IsError() || gate.calls != 0 {
				t.Fatalf("invalid edit reached write gate: %+v, %d", result, gate.calls)
			}
		})
	}
}

func TestWriteRechecksAfterGateAndRootRulesChange(t *testing.T) {
	for _, target := range []string{"a", "AGENTS.md", "sub/AGENTS.md"} {
		t.Run(target, func(t *testing.T) {
			gate := &applyGate{}
			f := setup(t, gate)
			put(t, f, "a", "before")
			put(t, f, "sub/a", "before")
			path := "a"
			if target == "sub/AGENTS.md" {
				path = "sub/a"
			}
			read := decode[ReadObservation](t, call(t, f, "read", map[string]any{"path": path}))
			gate.hook = func(workspace.Change) { put(t, f, target, "external") }
			if !call(t, f, "write", map[string]any{"path": path, "text": "after", "expected_hash": read.Hash}).IsError() {
				t.Fatal("change during approval was overwritten")
			}
			data, err := os.ReadFile(filepath.Join(f.dir, path))
			if err != nil || string(data) == "after" {
				t.Fatalf("unexpected disk state = %q, %v", data, err)
			}
		})
	}
}

func TestPathsRejectDirectoryLinksAndFIFO(t *testing.T) {
	f := setup(t, &applyGate{})
	put(t, f, "real/a", "before")
	if err := os.Symlink("real", filepath.Join(f.dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(f.dir, "pipe"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"link/a", "pipe", "real/../../escape"} {
		t.Run(path, func(t *testing.T) {
			if !call(t, f, "read", map[string]any{"path": path}).IsError() {
				t.Fatal("unsafe path accepted")
			}
		})
	}
}

type denyGate struct{}

func (denyGate) Commit(context.Context, workspace.Change, func() error) error { return ErrDenied }

func TestApprovalDenialDoesNotCreateFile(t *testing.T) {
	f := setup(t, denyGate{})
	decode[ReadObservation](t, call(t, f, "read", map[string]any{"path": "new"}))
	if !call(t, f, "write", map[string]any{"path": "new", "text": "after"}).IsError() {
		t.Fatal("denial not surfaced as soft error")
	}
	if _, err := os.Stat(filepath.Join(f.dir, "new")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("denied write changed disk: %v", err)
	}
}
