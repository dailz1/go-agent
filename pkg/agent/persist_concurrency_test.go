package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/dailz1/go-agent/pkg/llm"
	"github.com/dailz1/go-agent/pkg/store"
	"github.com/dailz1/go-agent/pkg/tool"
)

// TestSameThreadOwnershipAcrossAgents pins the ownership scope: two Agent
// instances sharing one store must not race the same thread into the
// provider. Agent A is held inside its provider call (deterministic signal,
// no sleeps); B's run on the same thread is rejected before touching the
// store; after A finishes, B can proceed.
func TestSameThreadOwnershipAcrossAgents(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	reg := tool.NewRegistry()
	reg.MustRegister(&mockTool{
		info:   tool.ToolInfo{Name: "echo", Description: "echoes"},
		result: tool.NewTextResult("echoed"),
	})

	gate := &gateProvider{
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
		reply:   llm.AssistantMessage("A done"),
	}
	agentA := New(gate, reg, WithStore(st), WithLogger(discardLogger()))
	agentB := persistTestAgent(t, st, MsgResponse(llm.AssistantMessage("B done")))

	doneA := make(chan error, 1)
	go func() {
		_, err := agentA.RunThread(ctx, "t", "a")
		doneA <- err
	}()
	// Bounded waits: a regression must fail the test, not hang the package.
	select {
	case <-gate.entered: // A holds ownership and is inside the provider call
	case <-time.After(2 * time.Second):
		t.Fatal("agent A never reached its provider call")
	}

	if _, err := agentB.RunThread(ctx, "t", "b"); !errors.Is(err, ErrThreadBusy) {
		t.Errorf("B RunThread error = %v, want ErrThreadBusy", err)
	}
	// B's rejection must not have written anything.
	equalKinds(t, logKinds(t, st, "t"), store.KindRunStarted)

	close(gate.release)
	select {
	case err := <-doneA:
		if err != nil {
			t.Fatalf("A RunThread: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("agent A never finished after release")
	}
	// Ownership is released with the run; B can now run the thread.
	if _, err := agentB.RunThread(ctx, "t", "b2"); err != nil {
		t.Errorf("B RunThread after A finished: %v", err)
	}
}

// TestGeneratedIDReservation pins rev=0 reservation semantics for
// kernel-generated IDs: a generated ID that already names a thread is a
// reservation conflict, retried with a fresh ID — never an append that
// contaminates the existing thread. The ID generator is a seam, so the
// collision is deterministic.
func TestGeneratedIDReservation(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	// Pre-seed a closed thread under the ID the generator will hand out first.
	head := stagedRunStarted(t, st, "gen-taken", "r-9", "existing")
	donePayload, err := json.Marshal(doneEventPayload{Message: llm.AssistantMessage("old")})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	doneRec, err := encodeRecord(store.KindAgentEvent, recordID("r-9", "-done"),
		eventEnvelope{Type: "done", Version: eventEnvelopeV1, Payload: donePayload})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	writeRaw(t, st, "gen-taken", head, doneRec)

	orig := newThreadID
	ids := []string{"gen-taken", "gen-fresh"}
	newThreadID = func() (string, error) {
		id := ids[0]
		ids = ids[1:]
		return id, nil
	}
	t.Cleanup(func() { newThreadID = orig })

	agent := persistTestAgent(t, st, MsgResponse(llm.AssistantMessage("new run")))
	res, err := agent.Run(ctx, "hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ThreadID != "gen-fresh" {
		t.Fatalf("ThreadID = %q, want the retry ID gen-fresh", res.ThreadID)
	}
	// The pre-existing thread is untouched: still exactly its closed run.
	equalKinds(t, logKinds(t, st, "gen-taken"), store.KindRunStarted, store.KindAgentEvent)
	// The fresh thread carries the new run.
	equalKinds(t, logKinds(t, st, "gen-fresh"), store.KindRunStarted, store.KindAgentEvent)
}

// TestGeneratedIDReservationActiveCollision pins that a generated ID
// colliding with a currently ACTIVE thread is also a reservation conflict,
// retried with a fresh ID — never a busy error and never an append to the
// active thread.
func TestGeneratedIDReservationActiveCollision(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	stagedRunStarted(t, st, "gen-active", "r-8", "in progress")

	orig := newThreadID
	ids := []string{"gen-active", "gen-fresh"}
	newThreadID = func() (string, error) {
		id := ids[0]
		ids = ids[1:]
		return id, nil
	}
	t.Cleanup(func() { newThreadID = orig })

	agent := persistTestAgent(t, st, MsgResponse(llm.AssistantMessage("new run")))
	res, err := agent.Run(ctx, "hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ThreadID != "gen-fresh" {
		t.Fatalf("ThreadID = %q, want the retry ID gen-fresh", res.ThreadID)
	}
	// The active thread is untouched.
	equalKinds(t, logKinds(t, st, "gen-active"), store.KindRunStarted)
}

// TestGeneratedIDCollisionWithOwnedThread pins the race the reservation
// protocol must prevent: the generated ID names a thread currently owned by
// a live run (held deterministically inside its provider call). That is a
// collision to retry, never a busy failure and never a write into the owned
// thread.
func TestGeneratedIDCollisionWithOwnedThread(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	reg := tool.NewRegistry()
	reg.MustRegister(&mockTool{
		info:   tool.ToolInfo{Name: "echo", Description: "echoes"},
		result: tool.NewTextResult("echoed"),
	})

	gate := &gateProvider{
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
		reply:   llm.AssistantMessage("A done"),
	}
	agentA := New(gate, reg, WithStore(st), WithLogger(discardLogger()))
	agentB := persistTestAgent(t, st, MsgResponse(llm.AssistantMessage("B done")))

	doneA := make(chan error, 1)
	go func() {
		_, err := agentA.RunThread(ctx, "owned", "a")
		doneA <- err
	}()
	select {
	case <-gate.entered: // A owns "owned" and is inside the provider call
	case <-time.After(2 * time.Second):
		t.Fatal("agent A never reached its provider call")
	}

	orig := newThreadID
	ids := []string{"owned", "gen-fresh"}
	newThreadID = func() (string, error) {
		id := ids[0]
		ids = ids[1:]
		return id, nil
	}
	t.Cleanup(func() { newThreadID = orig })

	res, err := agentB.Run(ctx, "hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ThreadID != "gen-fresh" {
		t.Fatalf("ThreadID = %q, want the retry ID gen-fresh", res.ThreadID)
	}
	// The owned thread is untouched by B.
	equalKinds(t, logKinds(t, st, "owned"), store.KindRunStarted)

	close(gate.release)
	select {
	case err := <-doneA:
		if err != nil {
			t.Fatalf("A RunThread: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("agent A never finished after release")
	}
}

// valueStore is a valid Store implementation with value receivers whose
// dynamic type contains a map and is therefore not comparable.
type valueStore struct {
	calls map[string]int
}

func (s valueStore) Append(_ context.Context, thread string, expected int64, records ...store.Record) (int64, error) {
	s.calls[thread]++
	return expected + int64(len(records)), nil
}

func (s valueStore) Latest(_ context.Context, _ string) (store.ThreadState, error) {
	return store.ThreadState{}, nil
}

func (s valueStore) History(_ context.Context, _ string, from int64) ([]store.Record, error) {
	return nil, nil
}

func (s valueStore) Delete(context.Context, string) error { return nil }

// ambiguousStore applies the first Append for real, then reports failure —
// the ambiguous outcome the idempotent reservation retry must resolve.
type ambiguousStore struct {
	store.Store
	armed bool
}

func (s *ambiguousStore) Append(ctx context.Context, thread string, expected int64, records ...store.Record) (int64, error) {
	if s.armed {
		s.armed = false
		if _, err := s.Store.Append(ctx, thread, expected, records...); err != nil {
			return 0, err
		}
		return 0, errors.New("applied but acknowledgement lost")
	}
	return s.Store.Append(ctx, thread, expected, records...)
}

// TestGeneratedIDReservationAmbiguousFailure pins the idempotent resolution
// of an ambiguous reservation: the record lands durably but the store
// reports failure; the byte-identical retry must adopt the reservation (same
// thread ID, no fresh-ID retry, no stranded active run).
func TestGeneratedIDReservationAmbiguousFailure(t *testing.T) {
	ctx := context.Background()
	st := &ambiguousStore{Store: store.NewMemory(), armed: true}

	orig := newThreadID
	newThreadID = func() (string, error) { return "gen-amb", nil }
	t.Cleanup(func() { newThreadID = orig })

	agent := persistTestAgent(t, st, MsgResponse(llm.AssistantMessage("x")))
	res, err := agent.Run(ctx, "hi")
	if err != nil {
		t.Fatalf("Run through ambiguous reservation: %v", err)
	}
	if res.ThreadID != "gen-amb" {
		t.Errorf("ThreadID = %q, want gen-amb (reservation adopted, not retried)", res.ThreadID)
	}
	// The ambiguous append and the idempotent retry did not duplicate the
	// reservation: exactly one run_started plus the terminal record.
	equalKinds(t, logKinds(t, st, "gen-amb"), store.KindRunStarted, store.KindAgentEvent)
}

// TestNonComparableStoreIsRejected pins that a store whose dynamic value is
// not comparable is rejected with a typed error instead of panicking in the
// ownership registry.
func TestNonComparableStoreIsRejected(t *testing.T) {
	reg := tool.NewRegistry()
	reg.MustRegister(&mockTool{
		info:   tool.ToolInfo{Name: "echo", Description: "echoes"},
		result: tool.NewTextResult("echoed"),
	})
	agent := New(NewMockProvider(MsgResponse(llm.AssistantMessage("x"))), reg,
		WithStore(valueStore{calls: map[string]int{}}), WithLogger(discardLogger()))
	if _, err := agent.RunThread(context.Background(), "t", "hi"); !errors.Is(err, ErrStoreNotComparable) {
		t.Fatalf("RunThread error = %v, want ErrStoreNotComparable", err)
	}
}

// public API: an escaped ID must still fit the JSONL file-name component
// limit, and a valid ID is accepted.
func TestThreadIDBoundaryValidation(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	agent := persistTestAgent(t, st, MsgResponse(llm.AssistantMessage("x")))

	if _, err := agent.RunThread(ctx, "", "hi"); !errors.Is(err, ErrInvalidThreadID) {
		t.Errorf("empty id error = %v, want ErrInvalidThreadID", err)
	}
	long := "/" + string(make([]byte, 200)) // escapes to 3 bytes per NUL
	if _, err := agent.RunThread(ctx, long, "hi"); !errors.Is(err, ErrInvalidThreadID) {
		t.Errorf("over-long escaped id error = %v, want ErrInvalidThreadID", err)
	}
	if _, err := agent.RunThread(ctx, "valid-thread-1", "hi"); err != nil {
		t.Errorf("valid id rejected: %v", err)
	}
}

// TestExplicitThreadAPIRequiresStore pins the fail-safe contract: the
// explicitly persistent entry points never silently degrade to a
// nonpersistent run.
func TestExplicitThreadAPIRequiresStore(t *testing.T) {
	reg := tool.NewRegistry()
	reg.MustRegister(&mockTool{
		info:   tool.ToolInfo{Name: "echo", Description: "echoes"},
		result: tool.NewTextResult("echoed"),
	})
	agent := New(NewMockProvider(MsgResponse(llm.AssistantMessage("x"))), reg, WithLogger(discardLogger()))
	if _, err := agent.RunThread(context.Background(), "t", "hi"); !errors.Is(err, ErrNoStore) {
		t.Errorf("RunThread without store = %v, want ErrNoStore", err)
	}
	if _, err := agent.RunThreadStream(context.Background(), "t", "hi"); !errors.Is(err, ErrNoStore) {
		t.Errorf("RunThreadStream without store = %v, want ErrNoStore", err)
	}
}
