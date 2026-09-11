package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/pkg/llm"
	"github.com/dailz1/go-agent/pkg/store"
	"github.com/dailz1/go-agent/pkg/tool"
)

// TestStreamLifecycleBarriers pins the lifecycle durability barriers of the
// streaming path: the declaration is durable when the first ToolCallEvent is
// delivered, the round is uncommitted while results are still being
// delivered (a crash there replays the batch as outcome unknown), and the
// terminal state is durable when DoneEvent is delivered.
func TestStreamLifecycleBarriers(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	reg := tool.NewRegistry()
	reg.MustRegister(&mockTool{
		info:   tool.ToolInfo{Name: "echo", Description: "echoes"},
		result: tool.NewTextResult("echoed"),
	})
	agent := New(NewMockStreamingProvider([][]llm.Chunk{
		{
			llm.ToolCallStartChunk{Index: 0, ID: "c1", Name: "echo"},
			llm.ToolCallArgsChunk{Index: 0, Delta: `{}`},
			llm.DoneChunk{FinishReason: "tool_calls"},
		},
		{
			llm.TextDeltaChunk{Text: "done"},
			llm.DoneChunk{FinishReason: "stop"},
		},
	}), reg, WithStore(st), WithLogger(discardLogger()))

	seq, err := agent.RunThreadStream(ctx, "t", "hi")
	if err != nil {
		t.Fatalf("RunThreadStream: %v", err)
	}
	sawCall, sawResult, sawDone := false, false, false
	for ev, iterErr := range seq {
		if iterErr != nil {
			t.Fatalf("stream error: %v", iterErr)
		}
		switch e := ev.(type) {
		case ToolCallEvent:
			sawCall = true
			if !hasKind(t, st, "t", store.KindRoundDeclared) {
				t.Error("ToolCallEvent delivered before the declaration was durable")
			}
			if e.ID != "c1" {
				t.Errorf("call ID = %q, want c1", e.ID)
			}
		case ToolResultEvent:
			sawResult = true
			if hasKind(t, st, "t", store.KindRoundCommitted) {
				t.Error("round committed while ToolResultEvent was still being delivered")
			}
		case DoneEvent:
			sawDone = true
			if !hasKind(t, st, "t", store.KindAgentEvent) {
				t.Error("DoneEvent delivered before the terminal record was durable")
			}
			if e.ThreadID != "t" {
				t.Errorf("DoneEvent.ThreadID = %q, want t", e.ThreadID)
			}
		}
	}
	if !sawCall || !sawResult || !sawDone {
		t.Fatalf("stream incomplete: call=%v result=%v done=%v", sawCall, sawResult, sawDone)
	}
}

func hasKind(t *testing.T, st store.Store, thread, kind string) bool {
	t.Helper()
	for _, r := range logRecords(t, st, thread) {
		if r.Kind == kind {
			return true
		}
	}
	return false
}

// compactKeepSystem keeps only the system prefix, standing in for a real
// compaction strategy that always shrinks below any budget.
type compactKeepSystem struct{}

func (compactKeepSystem) Compact(_ context.Context, history []llm.Message, _ CompactionBudget) (CompactionResult, error) {
	out := make([]llm.Message, 0, len(history))
	for _, m := range history {
		if m.Role == llm.RoleSystem {
			out = append(out, m)
		}
	}
	return CompactionResult{History: out, Changed: true, Strategies: []string{"keep-system"}}, nil
}

// TestCompactionPersistsCheckpoint pins that a compaction snapshot lands in
// the log as a checkpoint carrying the run lifecycle, and that replay from
// the snapshot yields exactly one system message — the frozen prompt is
// never duplicated by the seed.
func TestCompactionPersistsCheckpoint(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	reg := tool.NewRegistry()
	reg.MustRegister(&mockTool{
		info:   tool.ToolInfo{Name: "echo", Description: "echoes"},
		result: tool.NewTextResult("echoed"),
	})
	big := strings.Repeat("x", 400)
	agent := New(NewMockProvider(
		MsgResponse(toolCallMessage("c1")),
		MsgResponse(llm.AssistantMessage("done")),
	), reg,
		WithStore(st),
		WithSystemPrompt("thread system"),
		WithCompactor(compactKeepSystem{}),
		WithContextWindowTokens(200), // target ≈160 runes: the 400-rune input forces compaction
		WithLogger(discardLogger()),
	)
	if _, err := agent.RunThread(ctx, "t", big); err != nil {
		t.Fatalf("RunThread: %v", err)
	}
	state, err := st.Latest(ctx, "t")
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if state.Checkpoint == nil {
		t.Fatal("no checkpoint record after compaction")
	}
	var cp agentCheckpoint
	if err := strictDecode(state.Checkpoint.Payload, &cp); err != nil {
		t.Fatalf("checkpoint payload: %v", err)
	}
	if !cp.RunActive {
		t.Error("checkpoint run_active = false, want true (compaction happens mid-run)")
	}
	if cp.System != "thread system" {
		t.Errorf("checkpoint system = %q, want the thread's persisted prompt", cp.System)
	}

	// Replay takes the checkpoint fast path: the next run succeeds, the
	// model sees exactly one system message, and the compacted-away input
	// does not reappear.
	agent2 := persistTestAgent(t, st, MsgResponse(llm.AssistantMessage("next")))
	if _, err := agent2.RunThread(ctx, "t", "more"); err != nil {
		t.Fatalf("RunThread after compaction: %v", err)
	}
	msgs := agent2.provider.(*MockProvider).LastMessages
	systems := 0
	for _, m := range msgs {
		if m.Role == llm.RoleSystem {
			systems++
		}
		if m.Role == llm.RoleUser && messageString(m) == big {
			t.Error("compacted input was replayed despite the checkpoint")
		}
	}
	if systems != 1 {
		t.Errorf("replayed history has %d system messages, want exactly 1", systems)
	}
	if msgs[0].Role != llm.RoleSystem || messageString(msgs[0]) != "thread system" {
		t.Errorf("first message = %v %q, want the frozen system prompt", msgs[0].Role, messageString(msgs[0]))
	}
}

// TestRunThreadStreamIsLazy pins that a never-ranged RunThreadStream has no
// observable effect: no ownership, no replay, no input commit.
func TestRunThreadStreamIsLazy(t *testing.T) {
	st := store.NewMemory()
	agent := persistTestAgent(t, st, MsgResponse(llm.AssistantMessage("hi")))
	if _, err := agent.RunThreadStream(context.Background(), "t", "hi"); err != nil {
		t.Fatalf("RunThreadStream: %v", err)
	}
	state, err := st.Latest(context.Background(), "t")
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if state.Head != 0 {
		t.Errorf("head = %d after never-ranged stream, want 0", state.Head)
	}
}

// TestRunWithStoreGeneratesDistinctThreadIDs pins the Run sugar: each call
// gets a fresh kernel-generated thread ID, exposed on the result and
// discoverable in the store.
func TestRunWithStoreGeneratesDistinctThreadIDs(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	agent := persistTestAgent(t, st,
		MsgResponse(llm.AssistantMessage("one")),
		MsgResponse(llm.AssistantMessage("two")),
	)

	res1, err := agent.Run(ctx, "a")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	res2, err := agent.Run(ctx, "b")
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if res1.ThreadID == "" || res2.ThreadID == "" || res1.ThreadID == res2.ThreadID {
		t.Fatalf("thread IDs = %q, %q; want distinct nonempty", res1.ThreadID, res2.ThreadID)
	}
	for _, id := range []string{res1.ThreadID, res2.ThreadID} {
		kinds := logKinds(t, st, id)
		if len(kinds) != 2 || kinds[0] != store.KindRunStarted || kinds[1] != store.KindAgentEvent {
			t.Errorf("thread %s kinds = %v, want [run_started agent_event]", id, kinds)
		}
	}
}
