//go:build linux

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOutputLargePaginationReopen(t *testing.T) {
	s, _, store := shellFixture(t)
	got := executeShell(t, s, `awk 'BEGIN { for (i=0;i<350000;i++) print "0123456789012345678901234567890123456789012345678901234567890123456789" }'`)
	if got.Bytes <= 20<<20 || len(got.Head)+len(got.Tail) > 8<<10 || !got.Truncated {
		t.Fatalf("unbounded/missing output: %+v", got)
	}
	dir := store.dir
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenOutputStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var seam OutputReader = reopened
	page, err := seam.ReadOutput(t.Context(), got.OutputID, 349999, 1)
	if err != nil || len(page.Lines) != 1 || page.Lines[0].Number != 349999 ||
		!page.Truncated || page.NextOffset != 350000 {
		t.Fatalf("page: %+v %v", page, err)
	}
	last, err := seam.ReadOutput(t.Context(), got.OutputID, page.NextOffset, 10)
	if err != nil || len(last.Lines) != 1 || last.Truncated {
		t.Fatalf("last: %+v %v", last, err)
	}
	info, err := os.Stat(filepath.Join(dir, got.OutputID))
	if err != nil || info.Mode().Perm() != 0600 || info.Size() != got.Bytes {
		t.Fatalf("saved output: %v %v", info, err)
	}
}

func TestOutputQuotaAndInlineBudget(t *testing.T) {
	s, _, store := shellFixture(t)
	store.maxBytes = 1024
	WithShellInlineBudget(128)(s)
	got := executeShell(t, s, `while :; do printf '0123456789abcdef'; done`)
	if got.Outcome != "output_limit" || got.Bytes != 1024 || len(got.Head)+len(got.Tail) > 128 {
		t.Fatalf("quota: %+v", got)
	}
	info, err := os.Stat(filepath.Join(store.dir, got.OutputID))
	if err != nil || info.Size() != 1024 {
		t.Fatalf("file size: %v %v", info, err)
	}
	for _, id := range []string{"../outside", "/etc/passwd", "", strings.Repeat("a", 500)} {
		if _, err := store.ReadOutput(t.Context(), id, 1, 1); err == nil {
			t.Fatalf("accepted ID %q", id)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := store.ReadOutput(ctx, got.OutputID, 1, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled read: %v", err)
	}
}

func TestOutputDiskFailureStopsShell(t *testing.T) {
	s, _, store := shellFixture(t)
	store.newFile = func(string) (*os.File, error) { return os.OpenFile("/dev/full", os.O_WRONLY, 0) }
	result, err := s.Execute(t.Context(), json.RawMessage(`{"command":"while :; do printf 'data'; done"}`))
	if err == nil || result != nil {
		t.Fatalf("disk failure must remain infrastructure error: %v %v", result, err)
	}
}
