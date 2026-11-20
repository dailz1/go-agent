package agent

// Steering admission under an effective window (issue #17): idempotency
// confirmation must precede the size rejection, including records hidden
// behind a checkpoint, and the admission bound comes from the thread's
// current config projection.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

func TestSteeringEnqueueIdempotencyPrecedesAdmission(t *testing.T) {
	ctx := context.Background()
	pa := &windowedProvider{name: "pa", windows: map[string]int{"big-model": 100000},
		steps: [][]llm.Chunk{windowedText("a")}}
	st := store.NewMemory()
	ag := New(pa, tool.NewRegistry(),
		WithStore(st),
		WithContextWindowTokens(100000),
		WithLLMOptions(llm.WithModel("big-model")),
		WithMaxIter(3),
	)
	big := strings.Repeat("s", 3000)

	id, err := ag.EnqueueInputWithID(ctx, "t-s", "key-1", big)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := SetThreadConfig(ctx, st, "t-s", ConfigChange{
		Provider: windowPtr("pb"),
		Model:    windowPtr("small-model"),
	}); err != nil {
		t.Fatalf("switch setup: %v", err)
	}
	pb := &windowedProvider{name: "pb", windows: map[string]int{"small-model": 2000}}
	ag.providers["pb"] = pb

	retried, err := ag.EnqueueInputWithID(ctx, "t-s", "key-1", big)
	if err != nil {
		t.Fatalf("same-key retry after shrink: %v (want landed confirmation)", err)
	}
	if retried != id {
		t.Fatalf("same-key retry id = %q, want %q", retried, id)
	}

	_, err = ag.EnqueueInputWithID(ctx, "t-s", "key-2", big)
	if !errors.Is(err, ErrInputTooLarge) {
		t.Fatalf("new key over shrunk bound: want ErrInputTooLarge, got %v", err)
	}
	if !strings.Contains(fmt.Sprint(err), "2000") {
		t.Fatalf("rejection message must carry the effective bound 2000: %v", err)
	}
}

func TestSteeringEnqueueIdempotencyBehindCheckpoint(t *testing.T) {
	ctx := context.Background()
	pa := &windowedProvider{name: "pa", windows: map[string]int{"big-model": 100000},
		steps: [][]llm.Chunk{
			windowedText(strings.Repeat("a", 90000)),
			windowedText("b"),
		}}
	pb := &windowedProvider{name: "pb", windows: map[string]int{"small-model": 2000}}
	st := store.NewMemory()
	ag := New(pa, tool.NewRegistry(),
		WithStore(st),
		WithProviders(pb),
		WithContextWindowTokens(100000),
		WithLLMOptions(llm.WithModel("big-model")),
		WithMaxIter(3),
	)
	big := strings.Repeat("s", 3000)

	if _, err := ag.Run(ctx, "start", WithThread("t-b")); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	id, err := ag.EnqueueInputWithID(ctx, "t-b", "key-1", big)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := ag.Run(ctx, "go", WithThread("t-b")); err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if err := SetThreadConfig(ctx, st, "t-b", ConfigChange{
		Provider: windowPtr("pb"),
		Model:    windowPtr("small-model"),
	}); err != nil {
		t.Fatalf("switch: %v", err)
	}

	retried, err := ag.EnqueueInputWithID(ctx, "t-b", "key-1", big)
	if err != nil {
		t.Fatalf("same-key retry behind checkpoint: %v (want landed confirmation)", err)
	}
	if retried != id {
		t.Fatalf("same-key retry id = %q, want %q", retried, id)
	}
	_, err = ag.EnqueueInputWithID(ctx, "t-b", "key-2", big)
	if !errors.Is(err, ErrInputTooLarge) {
		t.Fatalf("new key over shrunk bound: want ErrInputTooLarge, got %v", err)
	}
}
