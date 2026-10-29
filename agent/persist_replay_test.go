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

// scriptStore replays a fixed record log; it exists to stage states the
// store's own Append would reject, such as record kinds or envelope versions
// a future binary might write.
type scriptStore struct {
	store.Store
	records []store.Record
}

func (s *scriptStore) Latest(_ context.Context, _ string) (store.ThreadState, error) {
	state := store.ThreadState{Head: int64(len(s.records))}
	for i := len(s.records) - 1; i >= 0; i-- {
		if s.records[i].Kind == store.KindCheckpoint {
			cp := s.records[i]
			state.Checkpoint = &cp
			state.Tail = s.records[i+1:]
			return state, nil
		}
	}
	state.Tail = s.records
	return state, nil
}

func (s *scriptStore) Append(_ context.Context, _ string, expected int64, records ...store.Record) (int64, error) {
	if expected != int64(len(s.records)) {
		return 0, store.ErrRevisionConflict
	}
	for i, r := range records {
		nr := r
		nr.Seq = expected + int64(i)
		s.records = append(s.records, nr)
	}
	return expected + int64(len(records)), nil
}

func (s *scriptStore) Delete(context.Context, string) error { return nil }

func (s *scriptStore) History(_ context.Context, _ string, from int64) ([]store.Record, error) {
	start := 0
	if from > 0 {
		if from >= int64(len(s.records)) {
			start = len(s.records)
		} else {
			start = int(from)
		}
	}
	return s.records[start:], nil
}

// TestReplayRejectsIncompatibleLogs pins the compatibility and lifecycle
// validation rules: unknown kinds, unknown envelope versions, and lifecycle
// records that contradict the run/round state machine abort replay with a
// typed error instead of being silently interpreted.
func TestReplayRejectsIncompatibleLogs(t *testing.T) {
	start, err := encodeRecord(store.KindRunStarted, "r-1-start",
		runStartedPayload{RunID: "r-1", Input: "go"})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	declared, err := encodeRecord(store.KindRoundDeclared, "r-1-d0",
		roundDeclaredPayload{RunID: "r-1", Round: 0, Message: toolCallMessage("c1")})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	start2, err := encodeRecord(store.KindRunStarted, "r-2-start",
		runStartedPayload{RunID: "r-2", Input: "second"})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	for _, tc := range []struct {
		name    string
		records []store.Record
	}{
		{
			name:    "unknown record kind",
			records: []store.Record{start, {Kind: "mystery_kind", Schema: 1, ID: "future", Payload: []byte(`{}`)}},
		},
		{
			name: "wrong envelope version",
			records: []store.Record{start, {Kind: store.KindAgentEvent, Schema: 1, ID: "env",
				Payload: []byte(`{"type":"done","version":2,"payload":{}}`)}},
		},
		{
			name: "declaration for another run",
			records: []store.Record{start, mustRecord(t, store.KindRoundDeclared, "r-other-d0",
				roundDeclaredPayload{RunID: "r-other", Round: 0, Message: toolCallMessage("c1")})},
		},
		{
			name: "commit without matching declaration",
			records: []store.Record{start, mustRecord(t, store.KindRoundCommitted, "r-1-c1",
				roundCommittedPayload{RunID: "r-1", Round: 1, Results: []llm.Message{
					llm.ToolResultMessage("c1", tool.NewErrorResult("x")),
				}})},
		},
		{
			name: "result batch does not pair with declaration",
			records: []store.Record{start, declared, mustRecord(t, store.KindRoundCommitted, "r-1-c0",
				roundCommittedPayload{RunID: "r-1", Round: 0, Results: []llm.Message{
					llm.ToolResultMessage("cOTHER", tool.NewErrorResult("x")),
				}})},
		},
		{
			name:    "run_started while a run is active",
			records: []store.Record{start, start2},
		},
		{
			name: "declaration skips a round number",
			records: []store.Record{start, mustRecord(t, store.KindRoundDeclared, "r-1-d1",
				roundDeclaredPayload{RunID: "r-1", Round: 1, Message: toolCallMessage("c1")})},
		},
		{
			name: "result count does not match declaration",
			records: []store.Record{start, declared, mustRecord(t, store.KindRoundCommitted, "r-1-c0",
				roundCommittedPayload{RunID: "r-1", Round: 0})},
		},
		{
			name: "done with a declaration still open",
			records: []store.Record{start, declared, mustRecord(t, store.KindAgentEvent, "r-1-done",
				eventEnvelope{Type: "done", Version: eventEnvelopeV1,
					Payload: mustJSON(t, doneEventPayload{Message: llm.AssistantMessage("x")})})},
		},
		{
			name: "truncated done re-carries the declared message",
			records: []store.Record{start, declared, mustRecord(t, store.KindRoundCommitted, "r-1-c0",
				roundCommittedPayload{RunID: "r-1", Round: 0, Results: []llm.Message{
					llm.ToolResultMessage("c1", tool.NewErrorResult("x")),
				}}),
				mustRecord(t, store.KindAgentEvent, "r-1-done",
					eventEnvelope{Type: "done", Version: eventEnvelopeV1,
						Payload: mustJSON(t, doneEventPayload{Message: toolCallMessage("c1"), Truncated: true})})},
		},
		{
			name: "declaration with an empty tool-call ID",
			records: []store.Record{start, mustRecord(t, store.KindRoundDeclared, "r-1-d0e",
				roundDeclaredPayload{RunID: "r-1", Round: 0, Message: toolCallMessage("")})},
		},
		{
			name: "declaration with duplicate tool-call IDs",
			records: []store.Record{start, mustRecord(t, store.KindRoundDeclared, "r-1-d0dup",
				roundDeclaredPayload{RunID: "r-1", Round: 0, Message: toolCallMessage("c1", "c1")})},
		},
		{
			name: "non-truncated done carrying tool calls",
			records: []store.Record{start, mustRecord(t, store.KindAgentEvent, "r-1-donetool",
				eventEnvelope{Type: "done", Version: eventEnvelopeV1,
					Payload: mustJSON(t, doneEventPayload{Message: toolCallMessage("c1")})})},
		},
		{
			name: "envelope version 0",
			records: []store.Record{start, {Kind: store.KindAgentEvent, Schema: 1, ID: "env0",
				Payload: []byte(`{"type":"done","version":0,"payload":{}}`)}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &scriptStore{records: tc.records}
			agent := persistTestAgent(t, st, MsgResponse(llm.AssistantMessage("x")))
			if _, err := agent.ResumeThread(context.Background(), "t"); !errors.Is(err, ErrIncompatibleLog) {
				t.Fatalf("ResumeThread error = %v, want ErrIncompatibleLog", err)
			}
		})
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func mustRecord(t *testing.T, kind, id string, payload any) store.Record {
	t.Helper()
	rec, err := encodeRecord(kind, id, payload)
	if err != nil {
		t.Fatalf("encode %s: %v", kind, err)
	}
	return rec
}

// enqueueAfterLatestStore deterministically stages the H1 interleaving: the
// first Latest call appends an input_queued record at the snapshot's head
// and then returns the pre-injection snapshot — an external enqueue that
// lands between the Latest read and the fallback History read.
type enqueueAfterLatestStore struct {
	store.Store
	injected bool
}

func (s *enqueueAfterLatestStore) Latest(ctx context.Context, thread string) (store.ThreadState, error) {
	state, err := s.Store.Latest(ctx, thread)
	if err != nil || s.injected {
		return state, err
	}
	s.injected = true
	rec, err := encodeRecord(store.KindInputQueued, "q-h1", inputQueuedPayload{Input: "concurrent-Q"})
	if err != nil {
		return store.ThreadState{}, err
	}
	rec.Schema = store.SchemaV2
	if _, err := s.Store.Append(ctx, thread, state.Head, rec); err != nil {
		return store.ThreadState{}, err
	}
	return state, nil
}

// stageH1Thread hand-builds a thread with an active run; a v2 checkpoint
// forces the full-log fallback path, and openDeclared adds an open round
// declaration so a resume must first write its recovery commit.
func stageH1Thread(t *testing.T, st store.Store, checkpoint, openDeclared bool) {
	t.Helper()
	head := stagedRunStarted(t, st, "t", "r1", "go")
	lastRound := -1
	if openDeclared {
		head = stagedDeclaration(t, st, "t", "r1", head, 0, toolCallMessage("c1"))
		lastRound = 0
	}
	if !checkpoint {
		return
	}
	// The checkpoint carries a valid v2 snapshot whose history omits the
	// open declaration; it is discarded on codec grounds either way.
	writeRaw(t, st, "t", head, store.Record{
		Kind: store.KindCheckpoint, Schema: store.SchemaV2, ID: "cp-h1",
		Payload: mustJSON(t, agentCheckpoint{
			CodecVersion: store.SchemaV2,
			History:      []llm.Message{llm.UserMessage("go")},
			System:       "",
			RunActive:    true,
			RunID:        "r1",
			LastRound:    lastRound,
		}),
	})
}

// TestReplayFallbackConcurrentEnqueueConsumedOnce pins the H1 contract:
// an input enqueued between the Latest snapshot and the fallback History
// read enters the projection exactly once. The v2-fallback cases were red
// before the fix — without open: the fallback replay and the first round
// top's refreshConfig each folded Q; with an open declaration: the fallback
// replay and the recovery commit's rebase each folded Q. The no-checkpoint
// cases are green sentinels: Latest.Tail and Latest.Head come from the same
// snapshot read, so no double-read window exists there.
func TestReplayFallbackConcurrentEnqueueConsumedOnce(t *testing.T) {
	const qText = "concurrent-Q"
	for _, tc := range []struct {
		name       string
		checkpoint bool
		open       bool
	}{
		{"v2_checkpoint/no_open", true, false},
		{"v2_checkpoint/open_round", true, true},
		{"no_checkpoint/no_open", false, false},
		{"no_checkpoint/open_round", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inner := store.NewMemory()
			stageH1Thread(t, inner, tc.checkpoint, tc.open)
			st := &enqueueAfterLatestStore{Store: inner}
			provider := NewMockStreamingProvider([][]llm.Chunk{
				textChunks("resumed"),
				textChunks("next run"),
			})
			agent := New(provider, tool.NewRegistry(), WithStore(st), WithLogger(discardLogger()))
			ctx := context.Background()
			if _, err := agent.ResumeThread(ctx, "t"); err != nil {
				t.Fatalf("ResumeThread: %v", err)
			}
			first := messageSliceString(provider.AllMessages[0])
			if got := strings.Count(first, qText); got != 1 {
				t.Fatalf("resumed outbound carries the enqueued input %d times, want 1: %q", got, first)
			}
			queued, consumed := 0, 0
			var drain inputConsumedPayload
			for _, r := range logRecords(t, st, "t") {
				switch r.Kind {
				case store.KindInputQueued:
					queued++
				case store.KindInputConsumed:
					consumed++
					if err := strictDecode(r.Payload, &drain); err != nil {
						t.Fatalf("decode input_consumed: %v", err)
					}
				}
			}
			if queued != 1 {
				t.Fatalf("input_queued records = %d, want 1", queued)
			}
			if consumed != 1 || len(drain.Inputs) != 1 ||
				drain.Inputs[0].ID != "q-h1" || drain.Inputs[0].Input != qText {
				t.Fatalf("input_consumed records = %d payload %+v, want 1 consuming exactly q-h1", consumed, drain.Inputs)
			}
			v, rerr := agent.replayThread(ctx, "t")
			if rerr != nil {
				t.Fatalf("post-run replay: %v (log must stay replayable)", rerr)
			}
			if len(v.queue) != 0 {
				t.Fatalf("post-run replay queue = %v, want empty", v.queue)
			}
			if _, err := agent.RunThread(ctx, "t", "next"); err != nil {
				t.Fatalf("RunThread after resume: %v", err)
			}
		})
	}
}

// TestReplayFallbackPrefixBoundedBySnapshotHead is the D-H1(a) white-box
// companion: replay of the fallback prefix terminates exactly at the Latest
// snapshot head, and records beyond it are never folded into the projection.
func TestReplayFallbackPrefixBoundedBySnapshotHead(t *testing.T) {
	inner := store.NewMemory()
	stageH1Thread(t, inner, true, false)
	st := &enqueueAfterLatestStore{Store: inner}
	agent := New(NewMockStreamingProvider(nil), tool.NewRegistry(), WithStore(st), WithLogger(discardLogger()))
	v, err := agent.replayThread(context.Background(), "t")
	if err != nil {
		t.Fatalf("replayThread: %v", err)
	}
	// The snapshot covered [run_started, checkpoint]: head 2, Q landed at 2.
	if v.head != 2 {
		t.Fatalf("view head = %d, want the snapshot head 2", v.head)
	}
	if len(v.queue) != 0 {
		t.Fatalf("queue = %v, want empty (records past the snapshot are left to absorption)", v.queue)
	}
	if !v.runActive {
		t.Fatal("view must keep the run active")
	}
}

func TestReplayRejectsRunClosedAfterCancellation(t *testing.T) {
	st := store.NewMemory()
	head := stagedRunStarted(t, st, "t", "r1", "go")
	cancelRec, err := encodeRecord(store.KindRunCancelled, recordID("r1", "-cancel"),
		runCancelledPayload{RunID: "r1", Results: []llm.Message{}})
	if err != nil {
		t.Fatal(err)
	}
	cancelRec.Schema = store.SchemaV2
	head = writeRaw(t, st, "t", head, cancelRec)
	closedRec, err := encodeRecord(store.KindRunClosed, recordID("r1", "-closed"),
		runClosedPayload{RunID: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	closedRec.Schema = store.SchemaV2
	writeRaw(t, st, "t", head, closedRec)
	agent := persistTestAgent(t, st)
	_, err = agent.replayThread(context.Background(), "t")
	if !errors.Is(err, ErrIncompatibleLog) || !strings.Contains(err.Error(), "outside the cooperative-end window") {
		t.Fatalf("replay err = %v, want ErrIncompatibleLog naming the cooperative-end window", err)
	}
}

func TestReplayRejectsUnknownDeclarationKind(t *testing.T) {
	st := store.NewMemory()
	stagedRunStarted(t, st, "t", "r1", "go")
	writeRaw(t, st, "t", 1, mustRecord(t, store.KindRoundDeclared, "r1-d0",
		roundDeclaredPayload{RunID: "r1", Round: 0, Message: toolCallMessage("c1"), Kind: "bogus"}))
	agent := persistTestAgent(t, st)
	_, err := agent.replayThread(context.Background(), "t")
	if !errors.Is(err, ErrIncompatibleLog) || !strings.Contains(err.Error(), `unknown declaration kind "bogus"`) {
		t.Fatalf("replay err = %v, want ErrIncompatibleLog naming the unknown declaration kind", err)
	}
}
