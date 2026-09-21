package snapshot

import (
	"errors"
	"testing"

	"github.com/dailz1/go-agent/harness/internal/workspace"
)

type injected struct{ point string }

func (e *injected) Error() string { return "injected at " + e.point }

func faultAt(point string) func(string) error {
	return func(p string) error {
		if p == point {
			return &injected{p}
		}
		return nil
	}
}

// A persistence fault during preparation must leave the workspace untouched
// and the store unusable until it is reopened.
func TestCommitFaultBeforeApplyPreventsWrite(t *testing.T) {
	for _, point := range []string{
		"prepared.file-sync.before", "prepared.file-sync.after",
		"prepared.rename.before", "prepared.rename.after",
		"prepared.dir-sync.before", "prepared.dir-sync.after",
	} {
		t.Run(point, func(t *testing.T) {
			s, w, root := fixture(t, 0)
			disk(t, root, "a", "before", 0644)
			s.fault = faultAt(point)
			c := workspace.Change{Path: "a", Before: "before", After: "after", Exists: true, Mode: 0644}
			calls := 0
			apply := func() error { calls++; return nil }
			if _, err := s.Commit(t.Context(), credential(w, 1), c, apply); err == nil || calls != 0 {
				t.Fatalf("fault at %s: err=%v calls=%d", point, err, calls)
			}
			image(t, root, "a", "before", 0644)
			c.After = "other"
			if _, err := s.Commit(t.Context(), credential(w, 2), c, apply); !errors.Is(err, ErrUncertain) {
				t.Fatalf("store stayed usable after fault: %v", err)
			}
			if calls != 0 {
				t.Fatal("apply ran after a persistence fault")
			}
		})
	}
}

// A fault after apply must report uncertainty, keep the applied image on disk,
// and remain resolvable by restore after reopening the store.
func TestCommitFaultAfterApplyKeepsEvidenceAndRestoreRecovers(t *testing.T) {
	for _, tc := range []struct {
		point     string
		persisted State
	}{
		{point: "applied.file-sync.before", persisted: Prepared},
		{point: "applied.dir-sync.after", persisted: Applied},
	} {
		t.Run(tc.point, func(t *testing.T) {
			s, w, root := fixture(t, 0)
			disk(t, root, "a", "before", 0600)
			s.fault = faultAt(tc.point)
			c := workspace.Change{Path: "a", Before: "before", After: "after", Exists: true, Mode: 0600}
			calls := 0
			_, err := s.Commit(t.Context(), credential(w, 1), c, func() error {
				calls++
				return w.Replace(t.Context(), c)
			})
			if err == nil || calls != 1 {
				t.Fatalf("err=%v calls=%d", err, calls)
			}
			image(t, root, "a", "after", 0600)
			reopened, oerr := Open(s.dir, w, "thread", 0)
			if oerr != nil {
				t.Fatal(oerr)
			}
			defer reopened.Close()
			records, lerr := reopened.List(t.Context())
			if lerr != nil || len(records) != 1 || records[0].State != tc.persisted {
				t.Fatalf("records=%+v %v", records, lerr)
			}
			if _, rerr := reopened.Restore(t.Context(), records[0].ID); rerr != nil {
				t.Fatal(rerr)
			}
			image(t, root, "a", "before", 0600)
		})
	}
}

// A fault while recording a completed restore must not modify the file a
// second time when the recovery restore is confirmed again.
func TestRestoreFaultRecoversWithoutSecondModification(t *testing.T) {
	for _, point := range []string{"restored.file-sync.before", "restored.rename.after"} {
		t.Run(point, func(t *testing.T) {
			s, w, root := fixture(t, 0)
			disk(t, root, "a", "before", 0644)
			c := workspace.Change{Path: "a", Before: "before", After: "after", Exists: true, Mode: 0644}
			r := commit(t, s, w, 1, c)
			image(t, root, "a", "after", 0644)
			s.fault = faultAt(point)
			if _, err := s.Restore(t.Context(), r.ID); err == nil {
				t.Fatalf("fault at %s not surfaced", point)
			}
			image(t, root, "a", "before", 0644)
			reopened, err := Open(s.dir, w, "thread", 0)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			for range 2 {
				got, rerr := reopened.Restore(t.Context(), r.ID)
				if rerr != nil || got.State != Restored {
					t.Fatalf("recovery restore = %+v, %v", got, rerr)
				}
				image(t, root, "a", "before", 0644)
			}
		})
	}
}
