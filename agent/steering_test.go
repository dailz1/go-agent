package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	if _, err := agent.Run(context.Background(), "compute", WithThread("t")); err != nil {
		t.Fatalf("Run: %v", err)
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
	if _, err := agent.Run(context.Background(), "new task", WithThread("t")); err != nil {
		t.Fatalf("Run: %v", err)
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
	if _, err := agent2.Run(context.Background(), "go", WithThread("t")); err != nil {
		t.Fatalf("Run: %v", err)
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
	// Admission has no fixed cap: the bound is the configured hard limit,
	// so an oversized input is staged against a small window.
	smallWindow := New(NewMockStreamingProvider(nil), tool.NewRegistry(),
		WithStore(store.NewMemory()),
		WithContextWindowTokens(200),
		WithCompactionThresholdPercent(50),
		WithLogger(discardLogger()),
	)
	big := strings.Repeat("x", 150)
	if _, err := smallWindow.EnqueueInputWithID(ctx, "t", "k", big); err == nil || !strings.Contains(err.Error(), "maximum length") {
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
	if _, err := agent.Run(ctx, "go", WithThread("t")); err != nil {
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

// countKind counts the thread's non-checkpoint records of one kind.
func countKind(t *testing.T, st store.Store, thread string, kind string) int {
	t.Helper()
	n := 0
	for _, r := range logRecords(t, st, thread) {
		if r.Kind == kind {
			n++
		}
	}
	return n
}

func TestSteeringConsumedRecordReplaysExactlyOnceOnResume(t *testing.T) {
	st := store.NewMemory()
	ctx := context.Background()
	head := stagedRunStarted(t, st, "t", "r1", "original")
	q, err := encodeRecord(store.KindInputQueued, "q-1", inputQueuedPayload{Input: "steered"})
	if err != nil {
		t.Fatal(err)
	}
	q.Schema = store.SchemaV2
	head = writeRaw(t, st, "t", head, q)
	c, err := encodeRecord(store.KindInputConsumed, "c-1",
		inputConsumedPayload{Inputs: []consumedInput{{ID: "q-1", Input: "steered"}}})
	if err != nil {
		t.Fatal(err)
	}
	c.Schema = store.SchemaV2
	writeRaw(t, st, "t", head, c)
	provider := NewMockStreamingProvider([][]llm.Chunk{textChunks("done")})
	agent := New(provider, tool.NewRegistry(), WithStore(st), WithLogger(discardLogger()))
	res, err := agent.ResumeThread(ctx, "t")
	if err != nil {
		t.Fatalf("ResumeThread: %v", err)
	}
	first := messageSliceString(provider.AllMessages[0])
	if got := strings.Count(first, "steered"); got != 1 {
		t.Fatalf("resumed outbound carries the consumed input %d times, want 1: %q", got, first)
	}
	userTurns := 0
	for _, m := range res.History {
		if m.Role == llm.RoleUser && messageString(m) == "steered" {
			userTurns++
		}
	}
	if userTurns != 1 {
		t.Fatalf("steered user turns in history = %d, want 1 (no replay re-insertion)", userTurns)
	}
	if got := countKind(t, st, "t", store.KindInputConsumed); got != 1 {
		t.Fatalf("input_consumed records = %d, want 1 (no second drain of the empty queue)", got)
	}
}

func TestSteeringEnqueuedInEndHookConsumedByNextRun(t *testing.T) {
	st := store.NewMemory()
	reg := tool.NewRegistry()
	reg.MustRegister(mutatorTool("calc", func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult("42"), nil
	}))
	provider := NewMockStreamingProvider([][]llm.Chunk{toolCallChunks("c1", "calc")})
	const qText = "queued in end hook"
	enqueued := false
	var agent *Agent
	agent = New(provider, reg, WithStore(st), WithLogger(discardLogger()),
		WithFinishRound(func(ctx context.Context, fc FinishRoundContext) (FinishRoundDecision, error) {
			if !enqueued {
				enqueued = true
				if _, err := agent.EnqueueInputWithID(ctx, fc.ThreadID, "k-end", qText); err != nil {
					return FinishRoundDecision{}, err
				}
			}
			return FinishRoundDecision{Action: FinishRoundEnd}, nil
		}),
	)
	res, err := agent.Run(context.Background(), "compute", WithThread("t"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(provider.AllMessages) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(provider.AllMessages))
	}
	if strings.Contains(messageSliceString(provider.AllMessages[0]), qText) {
		t.Fatal("the end-hook enqueue must not ride into this run's request")
	}
	if strings.Contains(messageSliceString(res.History), qText) {
		t.Fatal("the end-hook enqueue must not enter this run's history")
	}
	// The enqueue landed between the round commit and closeRun: the run_closed
	// append conflicts, absorbs the input, and closes behind it.
	equalKinds(t, logKinds(t, st, "t"),
		store.KindRunStarted, store.KindRoundDeclared, store.KindRoundCommitted,
		store.KindInputQueued, store.KindRunClosed)

	provider2 := NewMockStreamingProvider([][]llm.Chunk{textChunks("done2")})
	agent2 := New(provider2, tool.NewRegistry(), WithStore(st), WithLogger(discardLogger()))
	if _, err := agent2.Run(context.Background(), "next", WithThread("t")); err != nil {
		t.Fatalf("second run: %v", err)
	}
	second := messageSliceString(provider2.AllMessages[0])
	if got := strings.Count(second, qText); got != 1 {
		t.Fatalf("second-run outbound carries the queued input %d times, want 1: %q", got, second)
	}
	if got := countKind(t, st, "t", store.KindInputConsumed); got != 1 {
		t.Fatalf("input_consumed records = %d, want 1", got)
	}
}

// TestSteeringDeferralKeepsFIFOOrderNoSkip pins the FIFO no-skip rule: when
// the queue head does not fit the round's remaining budget, nothing behind
// it is consumed either. Serialized costs are calibrated against
// estimateRunes: round-1 history is 255 runes (no compaction at target 300),
// leaving 1245 available; Q1 costs 1331 (deferred), Q2 costs 75 (fits
// alone). Round 2 compacts the 428-rune history to 59 runes, leaving 1441
// >= 1331+75, so both land in FIFO order. A break→continue mutation in
// drainInputs would consume Q2 in round 1 and fail every assertion below.
func TestSteeringDeferralKeepsFIFOOrderNoSkip(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	reg := tool.NewRegistry()
	reg.MustRegister(mutatorTool("calc", func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult("42"), nil
	}))
	provider := NewMockStreamingProvider([][]llm.Chunk{
		toolCallChunks("c1", "calc"),
		textChunks("done"),
	})
	agent := New(provider, reg, WithStore(st),
		WithContextWindowTokens(1500),
		WithCompactionThresholdPercent(20),
		WithCompactor(CompactFunc(func(ctx context.Context, history []llm.Message, budget CompactionBudget) (CompactionResult, error) {
			return CompactionResult{History: []llm.Message{llm.UserMessage("seed")}, Changed: true}, nil
		})),
		WithLogger(discardLogger()),
	)
	q1 := strings.Repeat("z", 1276)
	q2 := strings.Repeat("y", 20)
	id1, err := agent.EnqueueInputWithID(ctx, "t", "k-q1", q1)
	if err != nil {
		t.Fatalf("enqueue q1: %v", err)
	}
	id2, err := agent.EnqueueInputWithID(ctx, "t", "k-q2", q2)
	if err != nil {
		t.Fatalf("enqueue q2: %v", err)
	}
	if _, err := agent.Run(ctx, strings.Repeat("x", 200), WithThread("t")); err != nil {
		t.Fatalf("run: %v", err)
	}
	round1 := messageSliceString(provider.AllMessages[0])
	if strings.Contains(round1, q1) || strings.Contains(round1, q2) {
		t.Fatalf("round-1 outbound must carry neither input (the head defers, so nothing behind it is consumed)")
	}
	round2 := messageSliceString(provider.AllMessages[1])
	if strings.Count(round2, q1) != 1 || strings.Count(round2, q2) != 1 {
		t.Fatalf("round-2 outbound must carry both inputs exactly once each")
	}
	msgs := provider.AllMessages[1]
	if n := len(msgs); n < 2 || messageString(msgs[n-1]) != q2 || messageString(msgs[n-2]) != q1 {
		var got string
		if n := len(msgs); n >= 2 {
			got = fmt.Sprintf("..., %q, %q", messageString(msgs[n-2]), messageString(msgs[n-1]))
		} else {
			got = fmt.Sprintf("%d message(s)", len(msgs))
		}
		t.Fatalf("round-2 tail order = %s; want Q1 then Q2", got)
	}
	var drains [][]consumedInput
	for _, r := range logRecords(t, st, "t") {
		if r.Kind != store.KindInputConsumed {
			continue
		}
		var p inputConsumedPayload
		if err := strictDecode(r.Payload, &p); err != nil {
			t.Fatalf("decode input_consumed: %v", err)
		}
		drains = append(drains, p.Inputs)
	}
	if len(drains) != 1 || len(drains[0]) != 2 ||
		drains[0][0].ID != string(id1) || drains[0][0].Input != q1 ||
		drains[0][1].ID != string(id2) || drains[0][1].Input != q2 {
		t.Fatalf("drains = %v, want one record consuming the FIFO prefix [Q1 Q2]", drains)
	}
	v, err := agent.replayThread(ctx, "t")
	if err != nil || len(v.queue) != 0 {
		t.Fatalf("post-run replay queue = %v (err %v), want empty", v.queue, err)
	}
}

// TestSteeringDrainConsumesPartialPrefix pins the partial-prefix drain: A
// fits, A+B does not, so round 1 consumes A without skipping over B to C.
// Calibrated costs: round-1 history 155 runes at target 230 leaves 995
// available; A costs 95, B costs 935 (A+B = 1030 > 995), C costs 85. Round 2
// compacts the 328-rune history to 59, leaving 1091 >= 935+85. A
// break→continue mutation would consume [A, C] in round 1 and fail.
func TestSteeringDrainConsumesPartialPrefix(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	reg := tool.NewRegistry()
	reg.MustRegister(mutatorTool("calc", func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult("42"), nil
	}))
	provider := NewMockStreamingProvider([][]llm.Chunk{
		toolCallChunks("c1", "calc"),
		textChunks("done"),
	})
	agent := New(provider, reg, WithStore(st),
		WithContextWindowTokens(1150),
		WithCompactionThresholdPercent(20),
		WithCompactor(CompactFunc(func(ctx context.Context, history []llm.Message, budget CompactionBudget) (CompactionResult, error) {
			return CompactionResult{History: []llm.Message{llm.UserMessage("seed")}, Changed: true}, nil
		})),
		WithLogger(discardLogger()),
	)
	a := strings.Repeat("a", 40)
	b := strings.Repeat("b", 880)
	c := strings.Repeat("c", 30)
	for _, q := range []struct {
		key, text string
	}{{"k-a", a}, {"k-b", b}, {"k-c", c}} {
		if _, err := agent.EnqueueInputWithID(ctx, "t", q.key, q.text); err != nil {
			t.Fatalf("enqueue %s: %v", q.key, err)
		}
	}
	if _, err := agent.Run(ctx, strings.Repeat("x", 100), WithThread("t")); err != nil {
		t.Fatalf("run: %v", err)
	}
	first := provider.AllMessages[0]
	if got := messageString(first[len(first)-1]); got != a {
		t.Fatalf("round-1 outbound tail = %q, want A only", got)
	}
	if joined := messageSliceString(first); strings.Contains(joined, b) || strings.Contains(joined, c) {
		t.Fatal("round-1 outbound must not carry B or C (the drain stops at B without skipping it)")
	}
	second := provider.AllMessages[1]
	n := len(second)
	if messageString(second[n-2]) != b || messageString(second[n-1]) != c {
		t.Fatalf("round-2 outbound tail = %q, %q; want B then C",
			messageString(second[n-2]), messageString(second[n-1]))
	}
	var drains [][]consumedInput
	for _, r := range logRecords(t, st, "t") {
		if r.Kind != store.KindInputConsumed {
			continue
		}
		var p inputConsumedPayload
		if err := strictDecode(r.Payload, &p); err != nil {
			t.Fatalf("decode input_consumed: %v", err)
		}
		drains = append(drains, p.Inputs)
	}
	if len(drains) != 2 || len(drains[0]) != 1 || drains[0][0].Input != a ||
		len(drains[1]) != 2 || drains[1][0].Input != b || drains[1][1].Input != c {
		t.Fatalf("drains = %v, want [[A] [B C]]", drains)
	}
	v, err := agent.replayThread(ctx, "t")
	if err != nil || len(v.queue) != 0 {
		t.Fatalf("post-run replay queue = %v (err %v), want empty", v.queue, err)
	}
}

func TestSteeringAdmissionMeasuresSerializedRunes(t *testing.T) {
	agent := New(NewMockStreamingProvider(nil), tool.NewRegistry(),
		WithStore(store.NewMemory()),
		WithContextWindowTokens(100),
		WithLogger(discardLogger()),
	)
	ctx := context.Background()
	// 40 raw newlines serialize to 135 runes (each escapes to two); 25
	// plain runes serialize to 80. A raw-rune admission check would wrongly
	// accept the first.
	if _, err := agent.EnqueueInputWithID(ctx, "t", "k-esc", strings.Repeat("\n", 40)); !errors.Is(err, ErrInputTooLarge) {
		t.Fatalf("escaped input err = %v, want ErrInputTooLarge (serialized measurement)", err)
	}
	if _, err := agent.EnqueueInputWithID(ctx, "t", "k-plain", strings.Repeat("x", 25)); err != nil {
		t.Fatalf("plain input err = %v", err)
	}
}

func TestSteeringLargeInputAcceptedUnderLargeWindow(t *testing.T) {
	st := store.NewMemory()
	agent := New(NewMockStreamingProvider(nil), tool.NewRegistry(),
		WithStore(st),
		WithContextWindowTokens(1_000_000),
		WithLogger(discardLogger()),
	)
	// 9_000 runes serialize to ~9_055: over the old fixed 8_192 cap, far
	// under the configured hard limit — admission must accept it. This is
	// the case that proves the fixed cap is gone; the reject tests below
	// pass under the old cap too.
	if _, err := agent.EnqueueInputWithID(context.Background(), "t", "k-large", strings.Repeat("x", 9000)); err != nil {
		t.Fatalf("large input under a large window err = %v, want nil", err)
	}
	if got := countKind(t, st, "t", store.KindInputQueued); got != 1 {
		t.Fatalf("input_queued records = %d, want 1", got)
	}
}

func TestSteeringInputOverHardLimitRejected(t *testing.T) {
	for _, tc := range []struct {
		name     string
		opts     []Option
		wantBind string
	}{
		{"window_bound", nil, "10000"},
		{"overlay_canonical_cap", []Option{
			WithRoundContextProvider(func(context.Context, RoundContextRequest) (string, error) { return "", nil }),
		}, "8000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := append([]Option{
				WithStore(store.NewMemory()),
				WithContextWindowTokens(10_000),
				WithLogger(discardLogger()),
			}, tc.opts...)
			agent := New(NewMockStreamingProvider(nil), tool.NewRegistry(), opts...)
			_, err := agent.EnqueueInputWithID(context.Background(), "t", "k", strings.Repeat("x", 20_000))
			if !errors.Is(err, ErrInputTooLarge) {
				t.Fatalf("err = %v, want ErrInputTooLarge", err)
			}
			// The printed bound must be the real binding limit, not a fixed constant.
			if !strings.Contains(err.Error(), tc.wantBind) {
				t.Fatalf("err = %v, want the actual admission bound %s in the message", err, tc.wantBind)
			}
		})
	}
}
