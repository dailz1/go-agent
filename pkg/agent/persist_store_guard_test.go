package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/dailz1/go-agent/pkg/llm"
	"github.com/dailz1/go-agent/pkg/store"
	"github.com/dailz1/go-agent/pkg/tool"
)

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
