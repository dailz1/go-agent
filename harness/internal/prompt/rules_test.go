package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/harness/internal/workspace"
)

func TestRulesPinRootAndDetectNestedChanges(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	put := func(path, text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, path), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	put("AGENTS.md", "root A")
	put("sub/AGENTS.md", "nested A")
	w, err := workspace.Open(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	r, err := Load(w, "", 8192)
	if err != nil {
		t.Fatal(err)
	}
	frozen := r.Snapshot()
	before, err := r.ForPath("sub/new.go")
	if err != nil || len(before.Sources) != 2 {
		t.Fatalf("applicable rules = %+v, %v", before, err)
	}
	put("sub/AGENTS.md", "nested B")
	after, err := r.ForPath("sub/new.go")
	if err != nil || before.Version == after.Version {
		t.Fatalf("nested rule change not detected: %v", err)
	}
	put("AGENTS.md", "root B")
	if !r.RootChanged() {
		t.Fatal("root change not detected")
	}
	if r.Snapshot().Version != frozen.Version || r.Snapshot().System != frozen.System {
		t.Fatal("frozen system silently swapped")
	}
}

func TestRulesRejectOversizeInsteadOfTruncating(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte(strings.Repeat("x", 8193)), 0600); err != nil {
		t.Fatal(err)
	}
	w, err := workspace.Open(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := Load(w, "", 8192); err == nil {
		t.Fatal("oversize rules accepted")
	}
}
