package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const roundResultPayload = `{"run_id":"run","round":2,"index":1,"result":{"role":"tool","content":[{"type":"tool_result","tool_use_id":"c1","content":"ok"}]}}`

func roundResultRec(id string, schema int, payload string) Record {
	if payload == "" {
		payload = roundResultPayload
	}
	return Record{Kind: KindRoundResult, Schema: schema, ID: id, Payload: json.RawMessage(payload)}
}

// TestRoundResultStoreContract pins the shared store contract for the new
// per-call result kind: schema normalization, full-content idempotent retry,
// conflict on divergent retries, and a JSONL reopen that keeps the record.
func TestRoundResultStoreContract(t *testing.T) {
	for _, backend := range []string{"memory", "jsonl"} {
		for _, schema := range []int{0, 1} {
			t.Run(backend+"/schema_"+string(rune('0'+schema)), func(t *testing.T) {
				ctx := t.Context()
				var s Store = NewMemory()
				var dir string
				if backend == "jsonl" {
					disk, d := newTestJSONL(t)
					s, dir = disk, d
				}
				r := roundResultRec("run-r2-1", schema, "")
				if head, err := s.Append(ctx, "t", 0, r); err != nil || head != 1 {
					t.Fatalf("append = %d, %v", head, err)
				}
				if head, err := s.Append(ctx, "t", 0, r); err != nil || head != 1 {
					t.Fatalf("identical retry = %d, %v", head, err)
				}
				// Same ID with different content must not silently win.
				other := roundResultRec("run-r2-1", 1, `{"run_id":"run","round":2,"index":1,"result":{"role":"tool","content":[{"type":"tool_result","tool_use_id":"c1","content":"changed"}]}}`)
				if _, err := s.Append(ctx, "t", 0, other); !errors.Is(err, ErrRevisionConflict) && !errors.Is(err, ErrInvalidRecord) {
					t.Fatalf("divergent retry = %v, want conflict or invalid", err)
				}
				got, err := s.History(ctx, "t", 0)
				if err != nil || len(got) != 1 {
					t.Fatalf("history = %+v, %v", got, err)
				}
				if got[0].Kind != KindRoundResult || got[0].Schema != SchemaV1 || got[0].ID != "run-r2-1" {
					t.Fatalf("round trip = %+v", got[0])
				}
				if !json.Valid(got[0].Payload) || !strings.Contains(string(got[0].Payload), `"index":1`) {
					t.Fatalf("payload = %s", got[0].Payload)
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
					again, err := reopened.History(ctx, "t", 0)
					if err != nil || len(again) != 1 || again[0].ID != "run-r2-1" {
						t.Fatalf("reopen history = %+v, %v", again, err)
					}
				}
			})
		}
	}
}

// TestRoundResultSchemaRejected pins the per-kind schema rule: round_result
// is schema 1 only, in both backends.
func TestRoundResultSchemaRejected(t *testing.T) {
	for _, backend := range []string{"memory", "jsonl"} {
		t.Run(backend, func(t *testing.T) {
			ctx := t.Context()
			var s Store = NewMemory()
			if backend == "jsonl" {
				disk, _ := newTestJSONL(t)
				s = disk
			}
			for _, schema := range []int{-1, 2, 3, 4} {
				bad := roundResultRec("run-r2-1", schema, "")
				if _, err := s.Append(ctx, "t", 0, bad); !errors.Is(err, ErrUnsupportedSchema) {
					t.Errorf("schema %d = %v, want unsupported", schema, err)
				}
			}
		})
	}
}

// TestRoundResultTornTailRecovery keeps the repaired-prefix guarantee: a
// torn final round_result line is truncated on reopen; earlier records and
// the acknowledged A record survive.
func TestRoundResultTornTailRecovery(t *testing.T) {
	ctx := t.Context()
	s, dir := newTestJSONL(t)
	if _, err := s.Append(ctx, "t", 0, rec("a", KindRunStarted), roundResultRec("run-r0-0", 1, "")); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "t.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Tear the tail mid-line: the JSONL repair must drop exactly this line.
	torn := append(data, []byte(`{"seq":2,"kind":"round_result","schema":1,"id":"run-r0-1","payload":{"run_i`)...)
	if err := os.WriteFile(path, torn, 0o644); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewJSONL(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	got, err := reopened.History(ctx, "t", 0)
	if err != nil || len(got) != 2 {
		t.Fatalf("history after repair = %+v, %v", got, err)
	}
	if got[1].ID != "run-r0-0" {
		t.Fatalf("surviving record = %+v, want the acknowledged A result", got[1])
	}
}
