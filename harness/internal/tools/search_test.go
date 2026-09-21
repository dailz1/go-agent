package tools

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestGlobRecursiveSortedPaginationAndExclusions(t *testing.T) {
	f := setup(t, nil)
	for _, name := range []string{"z.go", "a.go", "sub/b.go", "sub/deep/c.go", "vendor/hidden.go", ".env"} {
		put(t, f, name, "needle")
	}
	if err := os.Symlink("sub", filepath.Join(f.dir, "linked")); err != nil {
		t.Fatal(err)
	}
	got := decode[SearchObservation](t, call(t, f, "glob", map[string]any{"pattern": "**/*.go", "offset": 1, "limit": 2}))
	if !reflect.DeepEqual(got.Paths, []string{"a.go", "sub/b.go"}) || !got.Truncated || got.NextOffset != 3 {
		t.Fatalf("first page = %+v", got)
	}
	got = decode[SearchObservation](t, call(t, f, "glob", map[string]any{"pattern": "**/*.go", "offset": 3, "limit": 2}))
	if !reflect.DeepEqual(got.Paths, []string{"sub/deep/c.go", "z.go"}) || got.Truncated {
		t.Fatalf("second page = %+v", got)
	}
}

func TestGrepLongMatchMakesPaginationProgress(t *testing.T) {
	f := setup(t, nil)
	put(t, f, "a", strings.Repeat("x", observationBytes+1)+"\nx\n")
	got := decode[SearchObservation](t, call(t, f, "grep", map[string]any{"pattern": "x", "limit": 1}))
	if len(got.Matches) != 1 || !got.Truncated || got.NextOffset != 2 {
		t.Fatalf("long match must be bounded and pageable: %+v", got)
	}
}

func TestGrepEscapedContextCannotHideMatch(t *testing.T) {
	f := setup(t, nil)
	padding := strings.Repeat(strings.Repeat("\t", 1500)+"\n", 10)
	put(t, f, "a", padding+"needle\n"+padding)
	got := decode[SearchObservation](t, call(t, f, "grep", map[string]any{"pattern": "needle", "context": 10}))
	if len(got.Matches) != 1 || !got.Truncated {
		t.Fatalf("large encoded context hid the match: %+v", got)
	}
}

func TestGrepRegexpContextAndBudget(t *testing.T) {
	f := setup(t, nil)
	put(t, f, "sub/a.go", "before\nNEEDLE\nafter\n")
	put(t, f, ".env", "needle")
	got := decode[SearchObservation](t, call(t, f, "grep", map[string]any{
		"pattern": "need.e", "glob": "**/*.go", "ignore_case": true, "context": 1,
	}))
	if len(got.Matches) != 1 || got.Matches[0].Path != "sub/a.go" || got.Matches[0].Line != 2 ||
		len(got.Matches[0].Before) != 1 || len(got.Matches[0].After) != 1 {
		t.Fatalf("grep = %+v", got)
	}
	if !call(t, f, "grep", map[string]any{"pattern": "(?<=x)y"}).IsError() {
		t.Fatal("accepted unsupported regexp")
	}
	f.files.options.ScanBytes = 1
	got = decode[SearchObservation](t, call(t, f, "grep", map[string]any{"pattern": "absent"}))
	if !got.Truncated || got.Reason == "" {
		t.Fatal("budget exhaustion presented as complete absence")
	}
}
