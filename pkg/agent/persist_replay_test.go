package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/dailz1/go-agent/pkg/llm"
	"github.com/dailz1/go-agent/pkg/store"
	"github.com/dailz1/go-agent/pkg/tool"
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
