package store

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/dailz1/go-agent/llm"
)

// rec returns a record with the caller-owned fields filled and a valid
// placeholder payload.
func rec(id, kind string) Record {
	return Record{Kind: kind, Schema: SchemaV1, ID: id, Payload: json.RawMessage(`{"x":1}`)}
}

func cpRecord(id string, history ...llm.Message) Record {
	payload, err := json.Marshal(Checkpoint{History: history})
	if err != nil {
		panic(err)
	}
	return Record{Kind: KindCheckpoint, Schema: SchemaV1, ID: id, Payload: payload}
}

func threadName(i int) string {
	return "thread-" + string(rune('a'+i))
}

func messageText(msg *llm.Message) string {
	text := ""
	for _, block := range msg.Content {
		if tb, ok := block.(llm.TextBlock); ok {
			text += tb.Text
		}
	}
	return text
}

// exerciseContract runs the state/lifecycle family of the Store contract
// against any implementation: sequence assignment, checkpoints, thread
// deletion, and concurrency isolation.
func exerciseContract(t *testing.T, newStore func() Store) {
	t.Helper()
	ctx := context.Background()

	t.Run("append assigns sequential sequences from expected", func(t *testing.T) {
		s := newStore()
		head, err := s.Append(ctx, "t", 0, rec("a", KindRunStarted), rec("b", KindAgentEvent))
		if err != nil {
			t.Fatalf("Append: %v", err)
		}
		if head != 2 {
			t.Fatalf("head = %d, want 2", head)
		}
		state, err := s.Latest(ctx, "t")
		if err != nil {
			t.Fatalf("Latest: %v", err)
		}
		if state.Head != 2 || len(state.Tail) != 2 {
			t.Fatalf("state = head %d, %d records; want head 2, 2 records", state.Head, len(state.Tail))
		}
		if state.Tail[0].Seq != 0 || state.Tail[1].Seq != 1 {
			t.Errorf("seqs = %d,%d; want 0,1", state.Tail[0].Seq, state.Tail[1].Seq)
		}
		if state.Tail[0].Schema != SchemaV1 || state.Tail[0].RecordedAt.IsZero() {
			t.Errorf("store must fill schema and RecordedAt: %+v", state.Tail[0])
		}
	})

	t.Run("append with stale expected conflicts and writes nothing", func(t *testing.T) {
		s := newStore()
		if _, err := s.Append(ctx, "t", 0, rec("a", KindRunStarted)); err != nil {
			t.Fatalf("first append: %v", err)
		}
		if _, err := s.Append(ctx, "t", 0, rec("b", KindAgentEvent)); !errors.Is(err, ErrRevisionConflict) {
			t.Fatalf("stale append error = %v, want ErrRevisionConflict", err)
		}
		state, err := s.Latest(ctx, "t")
		if err != nil {
			t.Fatalf("Latest: %v", err)
		}
		if state.Head != 1 || len(state.Tail) != 1 {
			t.Errorf("conflicted append changed state: head %d, %d records", state.Head, len(state.Tail))
		}
	})

	t.Run("latest on unknown thread is empty at head 0", func(t *testing.T) {
		s := newStore()
		state, err := s.Latest(ctx, "missing")
		if err != nil {
			t.Fatalf("Latest: %v", err)
		}
		if state.Head != 0 || state.Checkpoint != nil || len(state.Tail) != 0 {
			t.Errorf("unknown thread state = %+v, want empty at head 0", state)
		}
	})

	t.Run("checkpoint splits tail at its own position", func(t *testing.T) {
		s := newStore()
		if _, err := s.Append(ctx, "t", 0,
			rec("e1", KindAgentEvent), rec("e2", KindAgentEvent),
			cpRecord("cp", llm.UserMessage("snap")), rec("e3", KindAgentEvent),
		); err != nil {
			t.Fatalf("append: %v", err)
		}
		state, err := s.Latest(ctx, "t")
		if err != nil {
			t.Fatalf("Latest: %v", err)
		}
		if state.Checkpoint == nil || state.Checkpoint.ID != "cp" {
			t.Fatalf("checkpoint = %+v, want cp", state.Checkpoint)
		}
		if len(state.Tail) != 1 || state.Tail[0].ID != "e3" || state.Tail[0].Seq != 3 {
			t.Errorf("tail = %+v, want only e3 at seq 3", state.Tail)
		}
		var cp Checkpoint
		if err := json.Unmarshal(state.Checkpoint.Payload, &cp); err != nil {
			t.Fatalf("checkpoint payload: %v", err)
		}
		if len(cp.History) != 1 || messageText(&cp.History[0]) != "snap" {
			t.Errorf("checkpoint history = %+v, want [snap]", cp.History)
		}
	})

	t.Run("no checkpoint means the whole log is tail", func(t *testing.T) {
		s := newStore()
		if _, err := s.Append(ctx, "t", 0, rec("e1", KindAgentEvent), rec("e2", KindAgentEvent)); err != nil {
			t.Fatalf("append: %v", err)
		}
		state, err := s.Latest(ctx, "t")
		if err != nil {
			t.Fatalf("Latest: %v", err)
		}
		if state.Checkpoint != nil || len(state.Tail) != 2 {
			t.Errorf("state = checkpoint %v, %d tail records; want none, 2", state.Checkpoint, len(state.Tail))
		}
	})

	t.Run("second checkpoint supersedes the first", func(t *testing.T) {
		s := newStore()
		if _, err := s.Append(ctx, "t", 0,
			rec("e1", KindAgentEvent), cpRecord("cp1", llm.UserMessage("one")),
			rec("e2", KindAgentEvent), cpRecord("cp2", llm.UserMessage("two")),
		); err != nil {
			t.Fatalf("append: %v", err)
		}
		state, err := s.Latest(ctx, "t")
		if err != nil {
			t.Fatalf("Latest: %v", err)
		}
		if state.Checkpoint == nil || state.Checkpoint.ID != "cp2" {
			t.Fatalf("checkpoint = %+v, want cp2", state.Checkpoint)
		}
		var cp Checkpoint
		if err := json.Unmarshal(state.Checkpoint.Payload, &cp); err != nil {
			t.Fatalf("checkpoint payload: %v", err)
		}
		if len(cp.History) != 1 || messageText(&cp.History[0]) != "two" {
			t.Errorf("checkpoint history = %+v, want [two]", cp.History)
		}
	})

	t.Run("delete removes the thread and is idempotent", func(t *testing.T) {
		s := newStore()
		if _, err := s.Append(ctx, "t", 0, rec("a", KindRunStarted)); err != nil {
			t.Fatalf("append: %v", err)
		}
		if err := s.Delete(ctx, "t"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if err := s.Delete(ctx, "t"); err != nil {
			t.Fatalf("repeated Delete: %v", err)
		}
		state, err := s.Latest(ctx, "t")
		if err != nil {
			t.Fatalf("Latest: %v", err)
		}
		if state.Head != 0 || len(state.Tail) != 0 {
			t.Errorf("deleted thread state = %+v, want empty", state)
		}
		if head, err := s.Append(ctx, "t", 0, rec("a2", KindRunStarted)); err != nil || head != 1 {
			t.Errorf("recreate after delete: head %d, err %v; want head 1, nil", head, err)
		}
	})

	t.Run("append rejects unknown kinds and empty ids", func(t *testing.T) {
		s := newStore()
		if _, err := s.Append(ctx, "t", 0, rec("a", "mystery")); !errors.Is(err, ErrUnknownKind) {
			t.Errorf("unknown kind error = %v, want ErrUnknownKind", err)
		}
		if _, err := s.Append(ctx, "t", 0, rec("", KindAgentEvent)); !errors.Is(err, ErrInvalidRecord) {
			t.Errorf("empty id error = %v, want ErrInvalidRecord", err)
		}
	})

	t.Run("canceled context aborts the operation", func(t *testing.T) {
		s := newStore()
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := s.Append(cctx, "t", 0, rec("a", KindRunStarted)); !errors.Is(err, context.Canceled) {
			t.Errorf("canceled append error = %v, want context.Canceled", err)
		}
		if _, err := s.Latest(cctx, "t"); !errors.Is(err, context.Canceled) {
			t.Errorf("canceled Latest error = %v, want context.Canceled", err)
		}
	})

	t.Run("concurrent appends to different threads both land", func(t *testing.T) {
		s := newStore()
		var wg sync.WaitGroup
		for i := range 8 {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				if _, err := s.Append(ctx, threadName(i), 0, rec("a", KindRunStarted)); err != nil {
					t.Errorf("thread %d append: %v", i, err)
				}
			}(i)
		}
		wg.Wait()
		for i := range 8 {
			state, err := s.Latest(ctx, threadName(i))
			if err != nil || state.Head != 1 {
				t.Errorf("thread %d: head %d, err %v; want head 1, nil", i, state.Head, err)
			}
		}
	})
}
