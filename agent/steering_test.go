package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

func TestSteeringInjectedNextRound(t *testing.T) {
	st := store.NewMemory()
	var agent *Agent
	reg := tool.NewRegistry()
	reg.MustRegister(mutatorTool("calc", func(ctx context.Context, _ json.RawMessage) (*tool.ToolResult, error) {
		if _, err := agent.EnqueueInputWithID(ctx, "t", "k1", "steer now"); err != nil {
			return nil, err
		}
		return tool.NewTextResult("42"), nil
	}))
	provider := NewMockStreamingProvider([][]llm.Chunk{
		toolCallChunks("c1", "calc"),
		textChunks("done"),
	})
	agent = New(provider, reg,
		WithStore(st),
		WithLogger(discardLogger()),
	)
	if _, err := agent.RunThread(context.Background(), "t", "compute"); err != nil {
		t.Fatalf("RunThread: %v", err)
	}
	if len(provider.AllMessages) != 2 {
		t.Fatalf("provider calls = %d, want 2", len(provider.AllMessages))
	}
	second := provider.AllMessages[1]
	last := second[len(second)-1]
	if last.Role != llm.RoleUser || messageString(last) != "steer now" {
		t.Fatalf("round-2 outbound tail = %v/%v, want steering user message", last.Role, messageString(last))
	}
}

func TestSteeringQueuedBetweenRunsConsumedFirstRound(t *testing.T) {
	st := store.NewMemory()
	agent := New(NewMockStreamingProvider([][]llm.Chunk{textChunks("done")}), tool.NewRegistry(),
		WithStore(st),
		WithLogger(discardLogger()),
	)
	if _, err := agent.EnqueueInputWithID(context.Background(), "t", "k1", "pre-queued"); err != nil {
		t.Fatalf("EnqueueInputWithID: %v", err)
	}
	if _, err := agent.RunThread(context.Background(), "t", "new task"); err != nil {
		t.Fatalf("RunThread: %v", err)
	}
	last := agent.lastOutboundTail(t)
	if last != "pre-queued" {
		t.Fatalf("first-round outbound tail = %q, want the queued message", last)
	}
	after, err := st.Latest(context.Background(), "t")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range after.Tail {
		if r.Kind == store.KindInputConsumed {
			found = true
		}
	}
	if !found {
		t.Fatal("input_consumed record missing")
	}
}

func (a *Agent) lastOutboundTail(t *testing.T) string {
	t.Helper()
	p, ok := a.provider.(*MockStreamingProvider)
	if !ok {
		t.Fatalf("provider is not a MockStreamingProvider")
	}
	last := p.LastMessages[len(p.LastMessages)-1]
	return messageString(last)
}

func TestSteeringDrainRebaseKeepsAbsorbedEntry(t *testing.T) {
	st := store.NewMemory()
	agent := New(NewMockStreamingProvider(nil), tool.NewRegistry(), WithStore(st), WithLogger(discardLogger()))
	if _, err := agent.EnqueueInputWithID(context.Background(), "t", "kA", "AAA first"); err != nil {
		t.Fatal(err)
	}
	injected := false
	wrapped := &injectOnConsumedStore{Store: st, inject: func(expected int64) (store.Record, int64, bool) {
		if injected {
			return store.Record{}, expected, false
		}
		injected = true
		rec, err := encodeRecord(store.KindInputQueued, "q-inject-B", inputQueuedPayload{Input: "BBB second"})
		if err != nil {
			t.Fatal(err)
		}
		rec.Schema = store.SchemaV2
		return rec, expected, true
	}}
	reg := tool.NewRegistry()
	reg.MustRegister(mutatorTool("calc", func(ctx context.Context, _ json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult("42"), nil
	}))
	provider := NewMockStreamingProvider([][]llm.Chunk{
		toolCallChunks("c1", "calc"),
		textChunks("r2"),
	})
	agent2 := New(provider, reg, WithStore(wrapped), WithLogger(discardLogger()))
	if _, err := agent2.RunThread(context.Background(), "t", "go"); err != nil {
		t.Fatalf("RunThread: %v", err)
	}
	if len(provider.AllMessages) != 2 {
		t.Fatalf("provider calls = %d, want 2", len(provider.AllMessages))
	}
	tail1 := messageString(provider.AllMessages[0][len(provider.AllMessages[0])-1])
	tail2 := messageString(provider.AllMessages[1][len(provider.AllMessages[1])-1])
	if tail1 != "AAA first" {
		t.Fatalf("round-1 tail = %q, want AAA first", tail1)
	}
	if tail2 != "BBB second" {
		t.Fatalf("round-2 tail = %q, want BBB second (absorbed during drain rebase)", tail2)
	}
}

type injectOnConsumedStore struct {
	store.Store
	inject func(expected int64) (store.Record, int64, bool)
}

func (s *injectOnConsumedStore) Append(ctx context.Context, thread string, expected int64, records ...store.Record) (int64, error) {
	if len(records) == 1 && records[0].Kind == store.KindInputConsumed {
		if rec, next, ok := s.inject(expected); ok {
			if _, err := s.Store.Append(ctx, thread, expected, rec); err != nil {
				return 0, err
			}
			expected = next
		}
	}
	return s.Store.Append(ctx, thread, expected, records...)
}

func TestSteeringAdmissionBound(t *testing.T) {
	agent := New(NewMockStreamingProvider(nil), tool.NewRegistry(),
		WithStore(store.NewMemory()),
		WithContextWindowTokens(200),
		WithCompactionThresholdPercent(50),
		WithLogger(discardLogger()),
	)
	over := strings.Repeat("x", 150)
	if _, err := agent.EnqueueInputWithID(context.Background(), "t", "k-big", over); !errors.Is(err, ErrInputTooLarge) {
		t.Fatalf("oversized vs admission bound err = %v, want ErrInputTooLarge", err)
	}
	fits := strings.Repeat("x", 30)
	if _, err := agent.EnqueueInputWithID(context.Background(), "t", "k-ok", fits); err != nil {
		t.Fatalf("within bound err = %v", err)
	}
}

func TestSteeringConsumedOutsideDrainWindowRejected(t *testing.T) {
	st := store.NewMemory()
	ctx := context.Background()
	q, err := encodeRecord(store.KindInputQueued, "q-1", inputQueuedPayload{Input: "x"})
	if err != nil {
		t.Fatal(err)
	}
	q.Schema = store.SchemaV2
	if _, err := st.Append(ctx, "t", 0, q); err != nil {
		t.Fatal(err)
	}
	c, err := encodeRecord(store.KindInputConsumed, "c-1", inputConsumedPayload{Inputs: []consumedInput{{ID: "q-1", Input: "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	c.Schema = store.SchemaV2
	if _, err := st.Append(ctx, "t", 1, c); err != nil {
		t.Fatal(err)
	}
	// 无 run 时消费：拒绝（ErrNothingToResume 之前 replay 先报不兼容）
	agent := New(NewMockStreamingProvider(nil), tool.NewRegistry(), WithStore(st), WithLogger(discardLogger()))
	v, rerr := agent.replayThread(ctx, "t")
	t.Logf("view: %+v err=%v", v, rerr)
	if _, rerr := agent.replayThread(ctx, "t"); rerr == nil {
		t.Fatal("consumed outside drain window accepted")
	} else if !strings.Contains(rerr.Error(), "drain window") {
		t.Fatalf("err = %v", rerr)
	}
}

func TestSteeringCrashBeforeDrainKeepsQueue(t *testing.T) {
	st := store.NewMemory()
	probe := New(NewMockStreamingProvider(nil), tool.NewRegistry(), WithStore(st), WithLogger(discardLogger()))
	if _, err := probe.EnqueueInputWithID(context.Background(), "t", "k1", "survives the crash"); err != nil {
		t.Fatal(err)
	}
	state, err := st.Latest(context.Background(), "t")
	if err != nil {
		t.Fatal(err)
	}
	rec, err := encodeRecord(store.KindRunStarted, "r1-start",
		runStartedPayload{RunID: "r1", Input: "original", SystemPrompt: ""})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(context.Background(), "t", state.Head, rec); err != nil {
		t.Fatal(err)
	}

	provider := NewMockStreamingProvider([][]llm.Chunk{textChunks("resumed")})
	agent := New(provider, tool.NewRegistry(), WithStore(st), WithLogger(discardLogger()))
	if _, err := agent.ResumeThread(context.Background(), "t"); err != nil {
		t.Fatalf("ResumeThread: %v", err)
	}
	first := provider.AllMessages[0]
	last := first[len(first)-1]
	if last.Role != llm.RoleUser || messageString(last) != "survives the crash" {
		t.Fatalf("resumed outbound tail = %v/%v, want queued steering message", last.Role, messageString(last))
	}
	after, err := st.Latest(context.Background(), "t")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range after.Tail {
		if r.Kind == store.KindInputConsumed {
			found = true
		}
	}
	if !found {
		t.Fatal("input_consumed record missing after drain")
	}
}

func TestEnqueueIdempotencyAndDistinctKeys(t *testing.T) {
	st := store.NewMemory()
	agent := New(NewMockStreamingProvider(nil), tool.NewRegistry(), WithStore(st), WithLogger(discardLogger()))
	ctx := context.Background()
	id1, err := agent.EnqueueInputWithID(ctx, "t", "k1", "same text")
	if err != nil {
		t.Fatal(err)
	}
	id2, err := agent.EnqueueInputWithID(ctx, "t", "k1", "same text")
	if err != nil || id2 != id1 {
		t.Fatalf("same key: id2=%v id1=%v err=%v, want identical", id2, id1, err)
	}
	id3, err := agent.EnqueueInputWithID(ctx, "t", "k2", "same text")
	if err != nil || id3 == id1 {
		t.Fatalf("distinct key must mint a distinct id, got %v / %v", id3, id1)
	}
	state, err := st.Latest(ctx, "t")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, r := range state.Tail {
		if r.Kind == store.KindInputQueued {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("input_queued records = %d, want 2", count)
	}
}

func TestEnqueueValidation(t *testing.T) {
	noStore := New(NewMockStreamingProvider(nil), tool.NewRegistry(), WithLogger(discardLogger()))
	if _, err := noStore.EnqueueInputWithID(context.Background(), "t", "k", "x"); err != ErrNoStore {
		t.Fatalf("err = %v, want ErrNoStore", err)
	}
	agent := New(NewMockStreamingProvider(nil), tool.NewRegistry(), WithStore(store.NewMemory()), WithLogger(discardLogger()))
	ctx := context.Background()
	if _, err := agent.EnqueueInputWithID(ctx, "t", "", "x"); err != ErrEmptyInput {
		t.Fatalf("empty key err = %v", err)
	}
	if _, err := agent.EnqueueInputWithID(ctx, "t", "k", ""); err != ErrEmptyInput {
		t.Fatalf("empty input err = %v", err)
	}
	big := strings.Repeat("x", MaxEnqueueInputRunes+1)
	if _, err := agent.EnqueueInputWithID(ctx, "t", "k", big); err == nil || !strings.Contains(err.Error(), "maximum length") {
		t.Fatalf("oversized err = %v", err)
	}
}

var _ = llm.TextDeltaChunk{}
var _ = json.RawMessage{}

func TestSteeringStarvationFreeDrain(t *testing.T) {
	st := store.NewMemory()
	provider := NewMockStreamingProvider([][]llm.Chunk{textChunks("done")})
	agent := New(provider, tool.NewRegistry(),
		WithStore(st),
		WithLogger(discardLogger()),
		WithContextWindowTokens(200),
		WithCompactionThresholdPercent(50),
		WithLogger(discardLogger()),
	)
	ctx := context.Background()
	input := strings.Repeat("x", 30)
	if _, err := agent.EnqueueInputWithID(ctx, "t", "k1", input); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := agent.RunThread(ctx, "t", "go"); err != nil {
		t.Fatalf("run: %v", err)
	}
	last := provider.LastMessages[len(provider.LastMessages)-1]
	if messageString(last) != input {
		t.Fatalf("drain-starved: outbound tail = %q, want the queued input", messageString(last))
	}
}

func messageSliceString(msgs []llm.Message) string {
	parts := make([]string, 0, len(msgs))
	for _, m := range msgs {
		parts = append(parts, messageString(m))
	}
	return strings.Join(parts, " | ")
}
