package store

import (
	"errors"
	"testing"
)

func TestSchemaV2Cancellation(t *testing.T) {
	for _, backend := range []string{"memory", "jsonl"} {
		t.Run(backend, func(t *testing.T) {
			var s Store = NewMemory()
			var dir string
			if backend == "jsonl" {
				var disk *JSONLStore
				disk, dir = newTestJSONL(t)
				s = disk
			}
			r := rec("r-cancel", "run_cancelled")
			r.Schema = 2
			if head, err := s.Append(t.Context(), "t", 0, r); err != nil || head != 1 {
				t.Fatalf("append v2 cancellation = %d, %v", head, err)
			}
			if head, err := s.Append(t.Context(), "t", 0, r); err != nil || head != 1 {
				t.Fatalf("retry v2 cancellation = %d, %v", head, err)
			}
			for _, schema := range []int{0, 1, 3, -1} {
				bad := r
				bad.Schema = schema
				if _, err := s.Append(t.Context(), "invalid", 0, bad); !errors.Is(err, ErrUnsupportedSchema) {
					t.Errorf("schema %d = %v, want unsupported", schema, err)
				}
			}
			if backend == "jsonl" {
				if err := s.(*JSONLStore).Close(); err != nil {
					t.Fatal(err)
				}
				reopened, err := NewJSONL(dir)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { reopened.Close() })
				s = reopened
			}
			got, err := s.History(t.Context(), "t", 0)
			if err != nil || len(got) != 1 || got[0].Schema != 2 || got[0].Kind != r.Kind {
				t.Fatalf("round trip = %+v, %v", got, err)
			}
		})
	}
}
