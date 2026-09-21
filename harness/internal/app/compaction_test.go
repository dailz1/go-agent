package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/dailz1/go-agent/llm"
)

// jsonRunes mirrors the kernel's history estimate for calibration.
func jsonRunes(t *testing.T, messages []llm.Message) int {
	t.Helper()
	encoded, err := json.Marshal(messages)
	if err != nil {
		t.Fatal(err)
	}
	return utf8.RuneCount(encoded)
}

// TestControllerCompactionSignalFromKernel proves the kernel's real
// CompactionEvent reaches the view model as the exposed notice: strategy
// names, rune deltas, and the swapped effective history. A recording pass
// measures the exact history sizes the kernel will see; the proving pass
// then picks a budget whose 80% threshold lands between the second and
// third round, so the oldest tool group is legitimately droppable.
func TestControllerCompactionSignalFromKernel(t *testing.T) {
	files := map[string]string{"big.txt": strings.Repeat("x", 80), "small.txt": strings.Repeat("y", 40)}
	replies := []stubReply{
		toolCall("c1", "read", `{"path":"big.txt"}`),
		toolCall("c2", "read", `{"path":"small.txt"}`),
		finalText("summarized"),
	}
	writeFiles := func(f *fixture) {
		t.Helper()
		for name, text := range files {
			if err := os.WriteFile(filepath.Join(f.startup.Workspace.Path(), name), []byte(text), 0o640); err != nil {
				t.Fatal(err)
			}
		}
	}

	// Recording pass: no compaction (huge budget, no truncation binding).
	rec := newFixtureWithOptions(t, fixtureOptions{noApproval: true,
		contextArg: []string{"--context-budget", "50000"}}, replies...)
	writeFiles(rec)
	rec.newSession()
	if err := rec.ctrl.Submit(rec.rootCtx, "read both files"); err != nil {
		t.Fatal(err)
	}
	rec.waitView(func(v ViewModel) bool { return v.Complete }, "recording run")
	rec.waitIdle("recording")
	if rec.prov.count() != 3 {
		t.Fatalf("recording calls = %d", rec.prov.count())
	}
	round2 := rec.prov.callAt(1).Messages // history before round 2
	round3 := rec.prov.callAt(2).Messages // history before round 3
	e2 := jsonRunes(t, round2)
	e3 := jsonRunes(t, round3)
	// Post-compaction estimate: drop the first tool group (assistant+result).
	post := append(append([]llm.Message{}, round3[:2]...), round3[4:]...)
	ePost := jsonRunes(t, post)
	if e3 <= e2 || e3 <= ePost {
		t.Fatalf("degenerate calibration: e2=%d e3=%d ePost=%d", e2, e3, ePost)
	}

	// Proving pass: threshold between round 2 and round 3.
	floor := max(e2, ePost) + 2
	budget := (floor*100 + 79) / 80 // smallest budget with 80% >= floor
	if target := (budget/100)*80 + (budget%100)*80/100; target >= e3-1 {
		t.Fatalf("calibration too tight: budget=%d target=%d e3=%d", budget, target, e3)
	}

	f := newFixtureWithOptions(t, fixtureOptions{noApproval: true,
		contextArg: []string{"--context-budget", strconv.Itoa(budget)}}, replies...)
	writeFiles(f)
	meta := f.newSession()
	if err := f.ctrl.Submit(f.rootCtx, "read both files"); err != nil {
		t.Fatal(err)
	}

	var notice *CompactionNotice
	complete := false
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for !(complete && notice != nil) {
		var view ViewModel
		select {
		case view = <-f.views:
		case <-deadline.C:
			t.Fatalf("timed out; complete=%v notice=%v", complete, notice)
		}
		if view.Interrupted {
			t.Fatalf("run interrupted instead of compacting: %+v", view)
		}
		if view.Compaction != nil {
			notice = view.Compaction
		}
		if view.Complete {
			complete = true
		}
	}
	f.waitIdle("compaction run")

	if notice.DroppedGroups < 1 || notice.AfterRunes >= notice.BeforeRunes {
		t.Fatalf("notice = %+v", notice)
	}
	if len(notice.Strategies) == 0 {
		t.Fatalf("no strategies reported: %+v", notice)
	}
	final := f.ctrl.View()
	if !final.Complete || len(final.History) == 0 {
		t.Fatalf("final view = %+v", final)
	}
	if final.Compaction == nil {
		t.Fatal("compaction signal lost from the final view")
	}
	cache, ok, err := f.mgr.LoadCache(meta.ID)
	if err != nil || !ok {
		t.Fatalf("cache after compacted run: ok=%v err=%v", ok, err)
	}
	if len(cache.History) == 0 || cache.Head == 0 {
		t.Fatalf("cache anchored to compacted run: %+v", cache)
	}
	_ = context.Background
}
