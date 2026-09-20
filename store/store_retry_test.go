package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// exerciseRetryContract pins the append/retry/validation family of the
// Store contract: revision matching, prefix idempotency, ID uniqueness,
// and input validation.
func exerciseRetryContract(t *testing.T, newStore func() Store) {
	t.Helper()
	ctx := context.Background()

	t.Run("retried append with same ids and content is idempotent", func(t *testing.T) {
		s := newStore()
		batch := []Record{rec("a", KindRunStarted), rec("b", KindAgentEvent)}
		if _, err := s.Append(ctx, "t", 0, batch...); err != nil {
			t.Fatalf("first append: %v", err)
		}
		head, err := s.Append(ctx, "t", 0, batch...)
		if err != nil {
			t.Fatalf("idempotent retry: %v", err)
		}
		if head != 2 {
			t.Fatalf("head = %d, want 2", head)
		}
		state, err := s.Latest(ctx, "t")
		if err != nil {
			t.Fatalf("Latest: %v", err)
		}
		if len(state.Tail) != 2 {
			t.Errorf("retry duplicated records: %d present", len(state.Tail))
		}
	})

	t.Run("retry completes a partially applied batch", func(t *testing.T) {
		s := newStore()
		// Simulate the ambiguous-crash state: the first record of the batch
		// is durable, the rest never landed.
		if _, err := s.Append(ctx, "t", 0, rec("a", KindRunStarted)); err != nil {
			t.Fatalf("partial apply: %v", err)
		}
		head, err := s.Append(ctx, "t", 0, rec("a", KindRunStarted), rec("b", KindAgentEvent))
		if err != nil {
			t.Fatalf("prefix retry: %v", err)
		}
		if head != 2 {
			t.Fatalf("head = %d, want 2", head)
		}
		state, err := s.Latest(ctx, "t")
		if err != nil {
			t.Fatalf("Latest: %v", err)
		}
		if len(state.Tail) != 2 || state.Tail[0].ID != "a" || state.Tail[1].ID != "b" {
			t.Errorf("tail = %+v, want exactly a,b", state.Tail)
		}
	})

	t.Run("retry after later records returns current head without writing", func(t *testing.T) {
		s := newStore()
		if _, err := s.Append(ctx, "t", 0, rec("a", KindRunStarted), rec("b", KindAgentEvent)); err != nil {
			t.Fatalf("append: %v", err)
		}
		if _, err := s.Append(ctx, "t", 2, rec("c", KindAgentEvent)); err != nil {
			t.Fatalf("later append: %v", err)
		}
		head, err := s.Append(ctx, "t", 0, rec("a", KindRunStarted), rec("b", KindAgentEvent))
		if err != nil {
			t.Fatalf("stale-batch retry: %v", err)
		}
		if head != 3 {
			t.Fatalf("head = %d, want 3 (current head)", head)
		}
		state, err := s.Latest(ctx, "t")
		if err != nil {
			t.Fatalf("Latest: %v", err)
		}
		if len(state.Tail) != 3 || state.Tail[2].ID != "c" {
			t.Errorf("retry mutated the log: %+v", state.Tail)
		}
	})

	t.Run("retry with changed content conflicts", func(t *testing.T) {
		s := newStore()
		if _, err := s.Append(ctx, "t", 0, rec("a", KindRunStarted)); err != nil {
			t.Fatalf("append: %v", err)
		}
		mutated := rec("a", KindRunStarted)
		mutated.Payload = json.RawMessage(`{"x":2}`)
		if _, err := s.Append(ctx, "t", 0, mutated); !errors.Is(err, ErrRevisionConflict) {
			t.Errorf("mutated payload retry error = %v, want ErrRevisionConflict", err)
		}
		kindSwap := rec("a", KindAgentEvent)
		if _, err := s.Append(ctx, "t", 0, kindSwap); !errors.Is(err, ErrRevisionConflict) {
			t.Errorf("kind-swap retry error = %v, want ErrRevisionConflict", err)
		}
		// An unset schema normalizes to SchemaV1, so this retry still matches.
		schemaSwap := rec("a", KindRunStarted)
		schemaSwap.Schema = 0
		if _, err := s.Append(ctx, "t", 0, schemaSwap); err != nil {
			t.Errorf("schema-normalized retry error = %v, want nil", err)
		}
	})

	t.Run("same ids at a different position conflict", func(t *testing.T) {
		s := newStore()
		if _, err := s.Append(ctx, "t", 0, rec("a", KindRunStarted), rec("b", KindAgentEvent)); err != nil {
			t.Fatalf("first append: %v", err)
		}
		if _, err := s.Append(ctx, "t", 1, rec("a", KindRunStarted)); !errors.Is(err, ErrRevisionConflict) {
			t.Fatalf("mispositioned retry error = %v, want ErrRevisionConflict", err)
		}
	})

	t.Run("duplicate ids inside one batch are rejected", func(t *testing.T) {
		s := newStore()
		if _, err := s.Append(ctx, "t", 0, rec("a", KindAgentEvent), rec("a", KindAgentEvent)); !errors.Is(err, ErrInvalidRecord) {
			t.Errorf("duplicate id error = %v, want ErrInvalidRecord", err)
		}
	})

	t.Run("an id already present elsewhere is rejected", func(t *testing.T) {
		s := newStore()
		if _, err := s.Append(ctx, "t", 0, rec("a", KindRunStarted)); err != nil {
			t.Fatalf("append: %v", err)
		}
		if _, err := s.Append(ctx, "t", 1, rec("a", KindAgentEvent)); !errors.Is(err, ErrInvalidRecord) {
			t.Errorf("id reuse error = %v, want ErrInvalidRecord", err)
		}
	})

	t.Run("unsupported schema versions are rejected at append", func(t *testing.T) {
		s := newStore()
		hi := rec("a", KindAgentEvent)
		hi.Schema = SchemaV1 + 1
		if _, err := s.Append(ctx, "t", 0, hi); !errors.Is(err, ErrUnsupportedSchema) {
			t.Errorf("schema V1+1 error = %v, want ErrUnsupportedSchema", err)
		}
		lo := rec("b", KindAgentEvent)
		lo.Schema = -3
		if _, err := s.Append(ctx, "t", 0, lo); !errors.Is(err, ErrUnsupportedSchema) {
			t.Errorf("negative schema error = %v, want ErrUnsupportedSchema", err)
		}
	})

	t.Run("malformed payload json is rejected", func(t *testing.T) {
		s := newStore()
		bad := rec("a", KindAgentEvent)
		bad.Payload = json.RawMessage(`{oops`)
		if _, err := s.Append(ctx, "t", 0, bad); !errors.Is(err, ErrInvalidRecord) {
			t.Errorf("malformed payload error = %v, want ErrInvalidRecord", err)
		}
	})

	t.Run("caller payload mutation after append is invisible", func(t *testing.T) {
		s := newStore()
		payload := json.RawMessage(`{"n":1}`)
		if _, err := s.Append(ctx, "t", 0, Record{Kind: KindAgentEvent, Schema: SchemaV1, ID: "a", Payload: payload}); err != nil {
			t.Fatalf("append: %v", err)
		}
		payload[4] = '9'
		state, err := s.Latest(ctx, "t")
		if err != nil {
			t.Fatalf("Latest: %v", err)
		}
		if string(state.Tail[0].Payload) != `{"n":1}` {
			t.Errorf("stored payload = %s, want original", state.Tail[0].Payload)
		}
	})

	t.Run("empty append validates the revision", func(t *testing.T) {
		s := newStore()
		if head, err := s.Append(ctx, "t", 0); err != nil || head != 0 {
			t.Errorf("fresh empty append = head %d, err %v; want head 0, nil", head, err)
		}
		if _, err := s.Append(ctx, "t", 0, rec("a", KindRunStarted)); err != nil {
			t.Fatalf("append: %v", err)
		}
		head, err := s.Append(ctx, "t", 0)
		if err != nil || head != 1 {
			t.Errorf("stale empty append = head %d, err %v; want head 1, nil", head, err)
		}
	})
}
