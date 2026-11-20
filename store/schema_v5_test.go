package store

import (
	"errors"
	"testing"
)

func TestSchemaV5KindCompatibility(t *testing.T) {
	for _, kind := range []string{
		KindRoundDeclared, KindAgentEvent, KindCheckpoint, KindRunStarted,
		KindRoundCommitted, KindRoundResult, KindRunCancelled, KindError,
		KindConfigChanged, KindInputQueued, KindInputConsumed, KindRunClosed,
		KindContextRecovery, KindHistoryReplaced,
	} {
		t.Run(kind, func(t *testing.T) {
			allowed := kind == KindRoundDeclared || kind == KindAgentEvent || kind == KindCheckpoint
			r := Record{Kind: kind, Schema: SchemaV5, ID: "v5", Payload: []byte(`{"codec_version":5}`)}
			for _, backend := range []string{"memory", "jsonl"} {
				t.Run(backend, func(t *testing.T) {
					var s Store = NewMemory()
					var dir string
					if backend == "jsonl" {
						disk, path := newTestJSONL(t)
						s, dir = disk, path
					}
					_, err := s.Append(t.Context(), "t", 0, r)
					if allowed && err != nil || !allowed && !errors.Is(err, ErrUnsupportedSchema) {
						t.Fatalf("schema 5 append %s: %v", kind, err)
					}
					if backend == "jsonl" {
						if !allowed {
							if err := appendRawRecord(dir, "t", r); err != nil {
								t.Fatal(err)
							}
						}
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
					log, err := s.History(t.Context(), "t", 0)
					if allowed {
						if err != nil || len(log) != 1 || log[0].Schema != SchemaV5 {
							t.Fatalf("schema 5 reopen: %+v, %v", log, err)
						}
					} else if backend == "jsonl" && !errors.Is(err, ErrUnsupportedSchema) {
						t.Fatalf("schema 5 illegal kind reopen: %v", err)
					}
				})
			}
		})
	}
}
