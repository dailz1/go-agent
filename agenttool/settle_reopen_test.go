package agenttool_test

import (
	"strings"
	"testing"

	"github.com/dailz1/go-agent/agent"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

func settleStores(t *testing.T, name string) (store.Store, store.Store) {
	t.Helper()
	var parent, child store.Store = store.NewMemory(), store.NewMemory()
	if strings.HasPrefix(name, "jsonl") {
		parent = settleJSONL(t, t.TempDir())
		child = settleJSONL(t, t.TempDir())
	}
	if strings.HasSuffix(name, "shared") {
		child = parent
	}
	return parent, child
}

func settleJSONL(t *testing.T, directory string) *store.JSONLStore {
	t.Helper()
	s, err := store.NewJSONL(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func TestSettleE58CrashBetweenChildAndParent(t *testing.T) {
	for _, name := range []string{"independent directories", "shared directory"} {
		t.Run(name, func(t *testing.T) {
			parentDir, childDir := t.TempDir(), t.TempDir()
			parentStore := settleJSONL(t, parentDir)
			childStore := parentStore
			if name == "independent directories" {
				childStore = settleJSONL(t, childDir)
			} else {
				childDir = parentDir
			}
			f := newSettleFixture(t, parentStore, childStore)
			parent, child, _ := f.interrupt(t)
			parentBefore := settleRecords(t, parentStore, parent.ThreadID)
			if err := f.child.SettleThread(t.Context(), child); err != nil {
				t.Fatal(err)
			}
			childBefore := settleRecords(t, childStore, child.ThreadID)
			settleUnchanged(t, parentStore, parent.ThreadID, parentBefore)
			if err := childStore.Close(); err != nil {
				t.Fatal(err)
			}
			if err := parentStore.Close(); err != nil {
				t.Fatal(err)
			}
			parentStore = settleJSONL(t, parentDir)
			childStore = parentStore
			if childDir != parentDir {
				childStore = settleJSONL(t, childDir)
			}
			// Empty scripts fail any accidental execution after reopening.
			parentProvider, childProvider := &roundContextSequenceProvider{}, &roundContextSequenceProvider{}
			reopenedParent := agent.New(parentProvider, tool.NewRegistry(), agent.WithStore(parentStore))
			reopenedChild := agent.New(childProvider, tool.NewRegistry(), agent.WithStore(childStore))
			h := settleHarness{
				parent: settleOwnedTarget{reopenedParent, parent}, expectedChildren: 1,
				children: []settleOwnedTarget{{reopenedChild, child}},
			}
			if _, err := h.cleanup(t.Context()); err != nil {
				t.Fatal(err)
			}
			settleUnchanged(t, childStore, child.ThreadID, childBefore)
			settleCancelled(t, reopenedChild, child, "leaf-call")
			settleCancelled(t, reopenedParent, parent, "parent-call")
			settleOneCancellation(t, parentStore, parent)
			settleOneCancellation(t, childStore, child)
			if len(parentProvider.requests) != 0 || len(childProvider.requests) != 0 || f.gate.calls.Load() != 1 {
				t.Fatal("crash recovery reexecuted provider/tool work")
			}
		})
	}
}
