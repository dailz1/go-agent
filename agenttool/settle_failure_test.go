package agenttool_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/agent"
	"github.com/dailz1/go-agent/store"
)

type settleFaultStore struct {
	store.Store
	mode string
	err  error
}

func (s *settleFaultStore) Append(ctx context.Context, id string, head int64, records ...store.Record) (int64, error) {
	if s.mode == "" || len(records) != 1 || records[0].Kind != store.KindRunCancelled {
		return s.Store.Append(ctx, id, head, records...)
	}
	mode := s.mode
	s.mode = ""
	if mode == "after write" {
		if _, err := s.Store.Append(ctx, id, head, records...); err != nil {
			return 0, err
		}
	}
	return 0, s.err
}

func TestSettleE57ChildCleanupFailureStopsRedirect(t *testing.T) {
	for _, name := range []string{"before write", "after write", "stale credential"} {
		t.Run(name, func(t *testing.T) {
			writeErr := errors.New("child cancel acknowledgement failed")
			childStore := &settleFaultStore{Store: store.NewMemory(), err: writeErr}
			f := newSettleFixture(t, store.NewMemory(), childStore)
			parent, child, _ := f.interrupt(t)
			wantErr := writeErr
			if name == "stale credential" {
				if _, err := f.child.ResumeThread(t.Context(), child.ThreadID); err != nil {
					t.Fatal(err)
				}
				seq, err := f.child.RunThreadStream(t.Context(), child.ThreadID, "new child task")
				if err != nil {
					t.Fatal(err)
				}
				for event, err := range seq {
					if err != nil {
						t.Fatal(err)
					}
					if _, ok := event.(agent.TextDeltaEvent); ok {
						break
					}
				}
				current, err := f.child.SettlementTarget(t.Context(), child.ThreadID)
				if err != nil || current == nil || current.RunID == child.RunID {
					t.Fatalf("failed to create successor child run: %+v, %v", current, err)
				}
				wantErr = store.ErrRevisionConflict
			} else {
				childStore.mode = name
			}
			parentBefore := settleRecords(t, f.parentStore, parent.ThreadID)
			childBefore := settleRecords(t, childStore, child.ThreadID)
			childCalls := len(f.childProvider.requests)
			h := settleHarness{
				parent: settleOwnedTarget{f.parent, parent}, expectedChildren: 1,
				children: []settleOwnedTarget{{f.child, child}},
			}
			pending, err := h.redirect(t.Context(), "redirect only after cleanup")
			if !errors.Is(err, wantErr) || pending != child.ThreadID || !strings.Contains(err.Error(), child.ThreadID) {
				t.Fatalf("cleanup failure not reported against child: pending=%q err=%v", pending, err)
			}
			settleUnchanged(t, f.parentStore, parent.ThreadID, parentBefore)
			if name != "after write" {
				settleUnchanged(t, childStore, child.ThreadID, childBefore)
			}
			if len(f.parentProvider.requests) != 1 || len(f.childProvider.requests) != childCalls {
				t.Fatal("failed child cleanup continued execution")
			}
			if name == "stale credential" {
				// An unchanged retry must still reject; never replace the saved target.
				if _, err := h.cleanup(t.Context()); !errors.Is(err, store.ErrRevisionConflict) {
					t.Fatalf("stale target was refreshed: %v", err)
				}
				settleUnchanged(t, childStore, child.ThreadID, childBefore)
				settleUnchanged(t, f.parentStore, parent.ThreadID, parentBefore)
				return
			}
			afterFailure := settleRecords(t, childStore, child.ThreadID)
			if _, err := h.cleanup(t.Context()); err != nil {
				t.Fatal(err)
			}
			if h.children[0].token != child {
				t.Fatal("retry changed the original credential")
			}
			if name == "after write" && !reflect.DeepEqual(afterFailure, settleRecords(t, childStore, child.ThreadID)) {
				t.Fatal("acknowledgement retry appended a second cancellation")
			}
			settleCancelled(t, f.child, child, "leaf-call")
			settleCancelled(t, f.parent, parent, "parent-call")
			settleOneCancellation(t, childStore, child)
			if len(f.parentProvider.requests) != 1 || len(f.childProvider.requests) != childCalls || f.gate.calls.Load() != 1 {
				t.Fatal("cleanup retry executed model/tool work")
			}
		})
	}
}
