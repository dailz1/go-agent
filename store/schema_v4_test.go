package store

import (
	"errors"
	"fmt"
	"testing"
)

func TestSchemaV4RecoveryRecords(t *testing.T) {
	for _, kind := range []string{KindContextRecovery, KindHistoryReplaced} {
		for _, backend := range []string{"memory", "jsonl"} {
			t.Run(kind+"/"+backend, func(t *testing.T) {
				var s Store = NewMemory()
				var dir string
				if backend == "jsonl" {
					var disk *JSONLStore
					disk, dir = newTestJSONL(t)
					s = disk
				}
				r := Record{Kind: kind, ID: "recovery", Schema: SchemaV4, Payload: []byte(`{"test":"payload"}`)}
				for _, schema := range []int{0, 1, 2, 3, 5} {
					t.Run(fmt.Sprintf("reject schema %d", schema), func(t *testing.T) {
						bad := r
						bad.Schema = schema
						if _, err := s.Append(t.Context(), "t", 0, bad); !errors.Is(err, ErrUnsupportedSchema) {
							t.Fatalf("schema %d: %v", schema, err)
						}
					})
				}
				t.Run("reject empty payload", func(t *testing.T) {
					bad := r
					bad.Payload = nil
					if _, err := s.Append(t.Context(), "empty", 0, bad); !errors.Is(err, ErrInvalidRecord) {
						t.Fatalf("empty payload: %v", err)
					}
				})
				if _, err := s.Append(t.Context(), "t", 0, r); err != nil {
					t.Fatal(err)
				}
				if backend == "jsonl" {
					if err := s.(*JSONLStore).Close(); err != nil {
						t.Fatal(err)
					}
					reopened, err := NewJSONL(dir)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() {
						if err := reopened.Close(); err != nil {
							t.Error(err)
						}
					})
					s = reopened
				}
				got, err := s.History(t.Context(), "t", 0)
				if err != nil || len(got) != 1 || got[0].Schema != SchemaV4 || got[0].Kind != kind {
					t.Fatalf("schema 4 round trip: %+v, %v", got, err)
				}
			})
		}
	}
}

func TestSchemaV4RecoveryReopenRejectsWrongSchema(t *testing.T) {
	for _, kind := range []string{KindContextRecovery, KindHistoryReplaced} {
		for _, schema := range []int{0, 1, 2, 3, 5} {
			t.Run(fmt.Sprintf("%s/schema %d", kind, schema), func(t *testing.T) {
				disk, dir := newTestJSONL(t)
				r := Record{Kind: kind, ID: "raw", Schema: schema, Payload: []byte(`{}`)}
				if err := appendRawRecord(dir, "t", r); err != nil {
					t.Fatal(err)
				}
				if err := disk.Close(); err != nil {
					t.Fatal(err)
				}
				reopened, err := NewJSONL(dir)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := reopened.Close(); err != nil {
						t.Error(err)
					}
				})
				if _, err := reopened.History(t.Context(), "t", 0); !errors.Is(err, ErrUnsupportedSchema) {
					t.Fatalf("wrong schema loaded: %v", err)
				}
			})
		}
	}
}

func TestSchemaV4LegacyKindsRejectedOnAppendAndReopen(t *testing.T) {
	for _, kind := range []string{
		KindRunStarted, KindRunCancelled, KindAgentEvent, KindRoundDeclared,
		KindRoundCommitted, KindRoundResult, KindError, KindConfigChanged,
		KindInputQueued, KindInputConsumed, KindRunClosed,
	} {
		t.Run(kind, func(t *testing.T) {
			r := Record{Kind: kind, ID: "legacy", Schema: SchemaV4, Payload: []byte(`{}`)}
			disk, dir := newTestJSONL(t)
			for name, s := range map[string]Store{"memory": NewMemory(), "jsonl": disk} {
				t.Run(name, func(t *testing.T) {
					if _, err := s.Append(t.Context(), "t", 0, r); !errors.Is(err, ErrUnsupportedSchema) {
						t.Fatalf("legacy schema 4 append: %v", err)
					}
				})
			}
			if err := disk.Close(); err != nil {
				t.Fatal(err)
			}
			if err := appendRawRecord(dir, "raw", r); err != nil {
				t.Fatal(err)
			}
			reopened, err := NewJSONL(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if _, err := reopened.History(t.Context(), "raw", 0); !errors.Is(err, ErrUnsupportedSchema) {
				t.Fatalf("legacy schema 4 reopen: %v", err)
			}
		})
	}
}

func TestSchemaV4RecoveryReopenRejectsMissingPayload(t *testing.T) {
	for _, kind := range []string{KindContextRecovery, KindHistoryReplaced} {
		t.Run(kind, func(t *testing.T) {
			disk, dir := newTestJSONL(t)
			if err := appendRawRecord(dir, "t", Record{Kind: kind, ID: "empty", Schema: SchemaV4}); err != nil {
				t.Fatal(err)
			}
			if err := disk.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := NewJSONL(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if _, err := reopened.History(t.Context(), "t", 0); !errors.Is(err, ErrCorruptLog) {
				t.Fatalf("missing payload accepted: %v", err)
			}
		})
	}
}
