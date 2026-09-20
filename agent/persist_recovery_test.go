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

// failAppendStore fails the n-th Append, simulating a durability crash at an
// exact commit point.
type failAppendStore struct {
	store.Store
	failOnCall int
	calls      int
}

func (f *failAppendStore) Append(ctx context.Context, thread string, expected int64, records ...store.Record) (int64, error) {
	f.calls++
	if f.calls == f.failOnCall {
		return 0, errors.New("disk on fire")
	}
	return f.Store.Append(ctx, thread, expected, records...)
}

// TestStoreFailureAbortsRoundAndResumeRecovers simulates a crash right at
// the round commit: the run fails loudly, the log holds the declaration
// without a commit, and resume resolves the round honestly.
func TestStoreFailureAbortsRoundAndResumeRecovers(t *testing.T) {
	ctx := context.Background()
	st := &failAppendStore{Store: store.NewMemory(), failOnCall: 3} // start, declared, COMMIT
	agent := persistTestAgent(t, st,
		MsgResponse(toolCallMessage("c1")),
		MsgResponse(llm.AssistantMessage("after recovery")),
	)

	_, err := agent.RunThread(ctx, "t", "hi")
	if err == nil || !strings.Contains(err.Error(), "disk on fire") {
		t.Fatalf("RunThread error = %v, want the storage failure", err)
	}
	equalKinds(t, logKinds(t, st, "t"), store.KindRunStarted, store.KindRoundDeclared)

	// Resume resolves the open declaration with outcome-unknown results and
	// finishes the run.
	if _, err := agent.ResumeThread(ctx, "t"); err != nil {
		t.Fatalf("ResumeThread: %v", err)
	}
	equalKinds(t, logKinds(t, st, "t"),
		store.KindRunStarted, store.KindRoundDeclared, store.KindRoundCommitted, store.KindAgentEvent)

	// The resumed model call saw the declaration plus its unknown result:
	// one role=tool IsError result for the declared call.
	msgs := agent.provider.(*MockProvider).LastMessages
	var tools int
	for _, m := range msgs {
		if m.Role == llm.RoleTool {
			tools++
			blocks := resultBlocks(m)
			if len(blocks) != 1 || !blocks[0].IsError || blocks[0].Content == "" {
				t.Errorf("synthesized result = %+v, want a non-empty IsError result", blocks)
			}
		}
	}
	if tools != 1 {
		t.Errorf("model saw %d tool results, want 1", tools)
	}
}

// TestCorruptCheckpointFallsBackToFullLog pins the H1 fallback: an
// undecodable checkpoint is discarded and the full log is read through
// History, which Latest cannot serve because it omits the checkpoint's
// prefix.
func TestCorruptCheckpointFallsBackToFullLog(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	head := stagedRunStarted(t, st, "t", "r-1", "go")
	head = stagedDeclaration(t, st, "t", "r-1", head, 0, toolCallMessage("c1"))
	// A checkpoint whose payload is valid JSON but not a checkpoint snapshot.
	cp := store.Record{Kind: store.KindCheckpoint, Schema: store.SchemaV1, ID: "cp-bad", Payload: []byte(`{"history":"bogus"}`)}
	head = writeRaw(t, st, "t", head, cp)
	// A durable result after the (now untrustworthy) checkpoint.
	rec, err := encodeRecord(store.KindRoundCommitted, recordID("r-1", "-c0"),
		roundCommittedPayload{RunID: "r-1", Round: 0, Results: []llm.Message{
			llm.ToolResultMessage("c1", tool.NewTextResult("echoed")),
		}})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	writeRaw(t, st, "t", head, rec)

	// Sanity: Latest alone would hide the run_started and declaration.
	state, err := st.Latest(ctx, "t")
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if len(state.Tail) != 1 {
		t.Fatalf("precondition: Latest tail = %d records, want 1", len(state.Tail))
	}

	agent := persistTestAgent(t, st, MsgResponse(llm.AssistantMessage("recovered")))
	if _, err := agent.ResumeThread(ctx, "t"); err != nil {
		t.Fatalf("ResumeThread with corrupt checkpoint: %v", err)
	}
	msgs := agent.provider.(*MockProvider).LastMessages
	// Full replay: user input, declared call, its committed result.
	if len(msgs) != 3 {
		t.Fatalf("replayed history = %d messages, want 3 (user, assistant, tool)", len(msgs))
	}
	if msgs[0].Role != llm.RoleUser || msgs[1].Role != llm.RoleAssistant || msgs[2].Role != llm.RoleTool {
		t.Errorf("replayed roles = %v, %v, %v", msgs[0].Role, msgs[1].Role, msgs[2].Role)
	}
}

// TestCheckpointSystemPrefixMismatchFallsBack pins that a decodable but
// structurally wrong checkpoint is discarded: an extra system message or a
// multi-block frozen prompt must not be trusted, and replay rebuilds from
// the full log instead.
func TestCheckpointSystemPrefixMismatchFallsBack(t *testing.T) {
	start, err := encodeRecord(store.KindRunStarted, "r-1-start",
		runStartedPayload{RunID: "r-1", Input: "go", SystemPrompt: "frozen"})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	for _, tc := range []struct {
		name    string
		history []llm.Message
	}{
		{
			name: "extra system message in the prefix",
			history: []llm.Message{
				llm.SystemMessage("frozen"),
				llm.SystemMessage("injected"),
			},
		},
		{
			name: "multi-block first system message",
			history: []llm.Message{
				{Role: llm.RoleSystem, Content: []llm.ContentBlock{
					llm.TextBlock{Type: "text", Text: "frozen"},
					llm.TextBlock{Type: "text", Text: " tail"},
				}},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cpState, err := json.Marshal(agentCheckpoint{History: tc.history, System: "frozen", RunActive: true, RunID: "r-1"})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			cp := store.Record{Kind: store.KindCheckpoint, Schema: store.SchemaV1, ID: "cp", Payload: cpState}
			st := &scriptStore{records: []store.Record{start, cp}}

			agent := persistTestAgent(t, st, MsgResponse(llm.AssistantMessage("recovered")))
			if _, err := agent.ResumeThread(context.Background(), "t"); err != nil {
				t.Fatalf("ResumeThread: %v", err)
			}
			// Full-log fallback: the model sees the original input behind the
			// single frozen system prompt, nothing injected.
			msgs := agent.provider.(*MockProvider).LastMessages
			if len(msgs) != 2 || msgs[0].Role != llm.RoleSystem || messageString(msgs[0]) != "frozen" {
				t.Fatalf("replayed prefix = %v, want the single frozen system message", roles(msgs))
			}
			if msgs[1].Role != llm.RoleUser || messageString(msgs[1]) != "go" {
				t.Errorf("second message = %v %q, want the original user input", msgs[1].Role, messageString(msgs[1]))
			}
		})
	}
}

// TestReplayKindErrorClosesOpenDeclaration pins the compatibility semantics:
// a terminal error record after a declaration closes the run AND clears the
// open declaration, so a later run replays cleanly instead of hitting a
// stale declaration.
func TestReplayKindErrorClosesOpenDeclaration(t *testing.T) {
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
	errRec := store.Record{Kind: store.KindError, Schema: 1, ID: "r-1-err", Payload: []byte(`{"any":"opaque"}`)}
	st := &scriptStore{records: []store.Record{start, declared, errRec}}

	agent := persistTestAgent(t, st, MsgResponse(llm.AssistantMessage("fresh run")))
	if _, err := agent.RunThread(context.Background(), "t", "again"); err != nil {
		t.Fatalf("RunThread after compatibility error record: %v", err)
	}
	// The model saw the interrupted declaration with its unresolved call
	// state left out (the error terminal is opaque) and then the new input —
	// no stale-open rejection.
	msgs := agent.provider.(*MockProvider).LastMessages
	if len(msgs) == 0 || msgs[len(msgs)-1].Role != llm.RoleUser {
		t.Fatalf("history does not end with the new user input: %v", roles(msgs))
	}
}

// TestReplayPreservesEmptyTerminalTurn pins that an empty non-truncated
// assistant terminal (a finish-only stream) survives the replay as a turn.
func TestReplayPreservesEmptyTerminalTurn(t *testing.T) {
	start, err := encodeRecord(store.KindRunStarted, "r-1-start",
		runStartedPayload{RunID: "r-1", Input: "go"})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	done, err := encodeRecord(store.KindAgentEvent, "r-1-done",
		eventEnvelope{Type: "done", Version: eventEnvelopeV1,
			Payload: mustJSON(t, doneEventPayload{Message: llm.Message{Role: llm.RoleAssistant}})})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	st := &scriptStore{records: []store.Record{start, done}}

	agent := persistTestAgent(t, st, MsgResponse(llm.AssistantMessage("next")))
	if _, err := agent.RunThread(context.Background(), "t", "again"); err != nil {
		t.Fatalf("RunThread: %v", err)
	}
	msgs := agent.provider.(*MockProvider).LastMessages
	want := []llm.Role{llm.RoleUser, llm.RoleAssistant, llm.RoleUser}
	if len(msgs) != len(want) {
		t.Fatalf("replayed history = %v, want %v (the empty terminal turn preserved)", roles(msgs), want)
	}
	for i := range want {
		if msgs[i].Role != want[i] {
			t.Errorf("replayed[%d] = %v, want %v", i, msgs[i].Role, want[i])
		}
	}
}

// TestReplayAcceptsCompatibleDone pins the positive path of the lifecycle
// validator: a coherent declaration/commit/terminal chain replays cleanly.
func TestReplayAcceptsCompatibleDone(t *testing.T) {
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
	committed, err := encodeRecord(store.KindRoundCommitted, "r-1-c0",
		roundCommittedPayload{RunID: "r-1", Round: 0, Results: []llm.Message{
			llm.ToolResultMessage("c1", tool.NewTextResult("echoed")),
		}})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	donePayload, err := json.Marshal(doneEventPayload{Message: llm.AssistantMessage("final")})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	done, err := encodeRecord(store.KindAgentEvent, "r-1-done",
		eventEnvelope{Type: "done", Version: eventEnvelopeV1, Payload: donePayload})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	st := &scriptStore{records: []store.Record{start, declared, committed, done}}

	agent := persistTestAgent(t, st, MsgResponse(llm.AssistantMessage("next")))
	if _, err := agent.RunThread(context.Background(), "t", "again"); err != nil {
		t.Fatalf("RunThread on replayed chain: %v", err)
	}
	msgs := agent.provider.(*MockProvider).LastMessages
	want := []llm.Role{llm.RoleUser, llm.RoleAssistant, llm.RoleTool, llm.RoleAssistant, llm.RoleUser}
	if len(msgs) != len(want) {
		t.Fatalf("replayed history = %v, want %v", roles(msgs), want)
	}
	for i := range want {
		if msgs[i].Role != want[i] {
			t.Errorf("replayed[%d] = %v, want %v", i, msgs[i].Role, want[i])
		}
	}
}
