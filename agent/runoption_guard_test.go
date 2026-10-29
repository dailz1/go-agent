package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

// appendCounter wraps a store and counts Append calls so a guard test can
// prove the store was never touched.
type appendCounter struct {
	store.Store
	appends int
}

func (s *appendCounter) Append(ctx context.Context, thread string, expected int64, records ...store.Record) (int64, error) {
	s.appends++
	return s.Store.Append(ctx, thread, expected, records...)
}

// TestRunWithoutWithThreadNeverTouchesStore pins the ephemeral guarantee:
// a store-configured agent runs ephemeral unless the call passes
// WithThread — no thread ID is generated and no record is written.
func TestRunWithoutWithThreadNeverTouchesStore(t *testing.T) {
	ctx := context.Background()
	st := &appendCounter{Store: store.NewMemory()}
	a := persistTestAgent(t, st,
		MsgResponse(llm.AssistantMessage("x")),
		MsgResponse(llm.AssistantMessage("y")),
	)

	res, err := a.Run(ctx, "hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ThreadID != "" {
		t.Errorf("ThreadID = %q, want \"\" for an ephemeral run", res.ThreadID)
	}
	if st.appends != 0 {
		t.Errorf("store Append calls = %d, want 0", st.appends)
	}

	seq, err := a.RunStream(ctx, "hi again")
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	done := 0
	for ev, err := range seq {
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		if d, ok := ev.(DoneEvent); ok {
			done++
			if d.ThreadID != "" {
				t.Errorf("DoneEvent.ThreadID = %q, want \"\" for an ephemeral run", d.ThreadID)
			}
		}
	}
	if done != 1 {
		t.Fatalf("done events = %d, want 1", done)
	}
	if st.appends != 0 {
		t.Errorf("store Append calls after stream = %d, want 0", st.appends)
	}
}

// TestWithThreadWithoutStoreFailsSynchronously pins the ErrNoStore contract
// for both the generate (empty) and the continue instruction: the error is
// returned before anything executes, and RunStream reports it directly
// instead of returning an iterator.
func TestWithThreadWithoutStoreFailsSynchronously(t *testing.T) {
	ctx := context.Background()
	reg := tool.NewRegistry()
	a := New(NewMockProvider(MsgResponse(llm.AssistantMessage("x"))), reg, WithLogger(discardLogger()))

	for _, id := range []string{"", "t-1"} {
		if _, err := a.Run(ctx, "hi", WithThread(id)); !errors.Is(err, ErrNoStore) {
			t.Errorf("Run(WithThread(%q)) error = %v, want ErrNoStore", id, err)
		}
		seq, err := a.RunStream(ctx, "hi", WithThread(id))
		if !errors.Is(err, ErrNoStore) {
			t.Errorf("RunStream(WithThread(%q)) error = %v, want ErrNoStore", id, err)
		}
		if seq != nil {
			t.Errorf("RunStream(WithThread(%q)) returned a non-nil iterator, want the error synchronously", id)
		}
	}
}

// TestWithThreadLastOneWins pins per-call override order: a later
// WithThread replaces an earlier one, matching constructor options.
func TestWithThreadLastOneWins(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	a := persistTestAgent(t, st, MsgResponse(llm.AssistantMessage("x")))

	res, err := a.Run(ctx, "hi", WithThread("thread-a"), WithThread("thread-b"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ThreadID != "thread-b" {
		t.Fatalf("ThreadID = %q, want thread-b (last WithThread wins)", res.ThreadID)
	}
	if kinds := logKinds(t, st, "thread-a"); len(kinds) != 0 {
		t.Errorf("thread-a record kinds = %v, want none", kinds)
	}
	equalKinds(t, logKinds(t, st, "thread-b"), store.KindRunStarted, store.KindAgentEvent)
}
