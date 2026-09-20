package store

import (
	"context"
	"errors"
	"testing"
)

// exerciseHistoryContract runs the full-log fallback family of the Store
// contract against any implementation: History must never omit a prefix a
// checkpoint would hide, because it is the recovery path when the newest
// checkpoint cannot be trusted.
func exerciseHistoryContract(t *testing.T, newStore func() Store) {
	t.Helper()
	ctx := context.Background()

	t.Run("history returns the full log including records a checkpoint hides", func(t *testing.T) {
		s := newStore()
		if _, err := s.Append(ctx, "t", 0,
			rec("e1", KindAgentEvent), rec("e2", KindAgentEvent),
			cpRecord("cp"), rec("e3", KindAgentEvent),
		); err != nil {
			t.Fatalf("append: %v", err)
		}
		state, err := s.Latest(ctx, "t")
		if err != nil {
			t.Fatalf("Latest: %v", err)
		}
		if len(state.Tail) != 1 {
			t.Fatalf("precondition: Latest tail = %d records, want 1 (checkpoint active)", len(state.Tail))
		}
		all, err := s.History(ctx, "t", 0)
		if err != nil {
			t.Fatalf("History: %v", err)
		}
		wantIDs := []string{"e1", "e2", "cp", "e3"}
		if len(all) != len(wantIDs) {
			t.Fatalf("History(0) = %d records, want %d", len(all), len(wantIDs))
		}
		for i, id := range wantIDs {
			if all[i].ID != id || all[i].Seq != int64(i) {
				t.Errorf("History(0)[%d] = %s@seq%d, want %s@seq%d", i, all[i].ID, all[i].Seq, id, i)
			}
		}
	})

	t.Run("history honors the from boundary", func(t *testing.T) {
		s := newStore()
		if _, err := s.Append(ctx, "t", 0,
			rec("e1", KindAgentEvent), rec("e2", KindAgentEvent), rec("e3", KindAgentEvent),
		); err != nil {
			t.Fatalf("append: %v", err)
		}
		for _, tc := range []struct {
			from  int64
			first string
			n     int
		}{
			{from: -5, first: "e1", n: 3},
			{from: 0, first: "e1", n: 3},
			{from: 1, first: "e2", n: 2},
			{from: 2, first: "e3", n: 1},
			{from: 3, n: 0},
			{from: 99, n: 0},
		} {
			got, err := s.History(ctx, "t", tc.from)
			if err != nil {
				t.Fatalf("History(%d): %v", tc.from, err)
			}
			if len(got) != tc.n {
				t.Errorf("History(%d) = %d records, want %d", tc.from, len(got), tc.n)
				continue
			}
			if tc.n > 0 && got[0].ID != tc.first {
				t.Errorf("History(%d)[0] = %s, want %s", tc.from, got[0].ID, tc.first)
			}
		}
	})

	t.Run("history on unknown thread is empty", func(t *testing.T) {
		s := newStore()
		got, err := s.History(ctx, "missing", 0)
		if err != nil || len(got) != 0 {
			t.Errorf("History(missing) = %d records, err %v; want 0, nil", len(got), err)
		}
	})

	t.Run("history returns copies the caller cannot use to mutate the store", func(t *testing.T) {
		s := newStore()
		if _, err := s.Append(ctx, "t", 0, rec("e1", KindAgentEvent)); err != nil {
			t.Fatalf("append: %v", err)
		}
		got, err := s.History(ctx, "t", 0)
		if err != nil {
			t.Fatalf("History: %v", err)
		}
		got[0].ID = "tampered"
		again, err := s.History(ctx, "t", 0)
		if err != nil {
			t.Fatalf("re-read: %v", err)
		}
		if again[0].ID != "e1" {
			t.Errorf("store record mutated through returned copy: %s", again[0].ID)
		}
	})

	t.Run("canceled context aborts History", func(t *testing.T) {
		s := newStore()
		cctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := s.History(cctx, "t", 0); !errors.Is(err, context.Canceled) {
			t.Errorf("canceled History error = %v, want context.Canceled", err)
		}
	})
}
