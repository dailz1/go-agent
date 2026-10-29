package store

import (
	"errors"
	"testing"
)

// Regression: the JSONL read side used to reject Schema > SchemaV2 while the
// write side accepted up to SchemaV3, so a persisted V3 checkpoint made the
// directory unreadable after reopen.
func TestSchemaV3CheckpointReopen(t *testing.T) {
	for _, backend := range []string{"memory", "jsonl"} {
		t.Run(backend, func(t *testing.T) {
			var s Store = NewMemory()
			var dir string
			if backend == "jsonl" {
				var disk *JSONLStore
				disk, dir = newTestJSONL(t)
				s = disk
			}
			r := rec("r-cp", "checkpoint")
			r.Schema = SchemaV3
			r.Payload = []byte(`{"CodecVersion":3}`)
			if head, err := s.Append(t.Context(), "t", 0, r); err != nil || head != 1 {
				t.Fatalf("append v3 checkpoint = %d, %v", head, err)
			}
			if backend == "jsonl" {
				if err := s.(*JSONLStore).Close(); err != nil {
					t.Fatal(err)
				}
				reopened, err := NewJSONL(dir)
				if err != nil {
					t.Fatalf("reopen with v3 checkpoint: %v", err)
				}
				t.Cleanup(func() { reopened.Close() })
				s = reopened
			}
			got, err := s.History(t.Context(), "t", 0)
			if err != nil || len(got) != 1 || got[0].Schema != SchemaV3 || got[0].ID != "r-cp" {
				t.Fatalf("round trip = %+v, %v", got, err)
			}
		})
	}
}

// The read-side gate must still reject schemas newer than the store knows.
func TestSchemaV4ReopenRejected(t *testing.T) {
	disk, dir := newTestJSONL(t)
	r := rec("r-future", "agent_event")
	r.Schema = SchemaV3 + 1
	if _, err := disk.Append(t.Context(), "t", 0, r); !errors.Is(err, ErrUnsupportedSchema) {
		t.Fatalf("append future schema = %v, want unsupported", err)
	}
	rFuture := rec("t4", "agent_event")
	rFuture.Schema = SchemaV3 + 1
	if err := appendRawRecord(dir, "t", rFuture); err != nil {
		t.Fatalf("seed raw: %v", err)
	}
	if err := disk.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewJSONL(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { reopened.Close() })
	if _, err := reopened.History(t.Context(), "t", 0); !errors.Is(err, ErrUnsupportedSchema) {
		t.Fatalf("history with future schema = %v, want unsupported", err)
	}
}
