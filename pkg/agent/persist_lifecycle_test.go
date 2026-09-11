package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/dailz1/go-agent/pkg/llm"
	"github.com/dailz1/go-agent/pkg/store"
	"github.com/dailz1/go-agent/pkg/tool"
)

// TestRunThreadPersistsRoundLifecycle pins the record shape of a complete
// tool round and the replay of that history into the next run.
func TestRunThreadPersistsRoundLifecycle(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	agent := persistTestAgent(t, st,
		MsgResponse(toolCallMessage("c1")),
		MsgResponse(llm.AssistantMessage("all done")),
	)

	res, err := agent.RunThread(ctx, "t1", "hi")
	if err != nil {
		t.Fatalf("RunThread: %v", err)
	}
	if res.ThreadID != "t1" {
		t.Errorf("ThreadID = %q, want t1", res.ThreadID)
	}
	equalKinds(t, logKinds(t, st, "t1"),
		store.KindRunStarted, store.KindRoundDeclared, store.KindRoundCommitted, store.KindAgentEvent)

	records := logRecords(t, st, "t1")
	var declared roundDeclaredPayload
	if err := strictDecode(records[1].Payload, &declared); err != nil {
		t.Fatalf("declared payload: %v", err)
	}
	if len(toolUseBlocks(declared.Message)) != 1 || toolUseBlocks(declared.Message)[0].ID != "c1" {
		t.Errorf("declared message = %+v, want one call c1", declared.Message)
	}
	var committed roundCommittedPayload
	if err := strictDecode(records[2].Payload, &committed); err != nil {
		t.Fatalf("committed payload: %v", err)
	}
	if len(committed.Results) != 1 {
		t.Fatalf("committed results = %d, want 1", len(committed.Results))
	}
	blocks := resultBlocks(committed.Results[0])
	if len(blocks) != 1 || blocks[0].ToolUseID != "c1" || blocks[0].IsError {
		t.Errorf("result = %+v, want non-error result for c1", blocks)
	}
	var done doneEventPayload
	if err := strictDecode(envelopeOf(t, records[3]).Payload, &done); err != nil {
		t.Fatalf("done payload: %v", err)
	}
	if done.Truncated {
		t.Error("done.Truncated = true, want false")
	}
	if len(done.Message.Content) == 0 || messageString(done.Message) != "all done" {
		t.Errorf("done message = %q, want the final reply", messageString(done.Message))
	}

	// The next run replays the closed history: the model must see the whole
	// prior conversation, not just the new input.
	agent2 := persistTestAgent(t, st, MsgResponse(llm.AssistantMessage("second")))
	if _, err := agent2.RunThread(ctx, "t1", "again"); err != nil {
		t.Fatalf("second RunThread: %v", err)
	}
	msgs := agent2.provider.(*MockProvider).LastMessages
	if len(msgs) != 5 {
		t.Fatalf("replayed history = %d messages, want 5 (user,assistant,result,final,user)", len(msgs))
	}
	if msgs[0].Role != llm.RoleUser || msgs[1].Role != llm.RoleAssistant || msgs[2].Role != llm.RoleTool {
		t.Errorf("replayed roles = %v %v %v", msgs[0].Role, msgs[1].Role, msgs[2].Role)
	}
	if msgs[3].Role != llm.RoleAssistant || messageString(msgs[3]) != "all done" {
		t.Errorf("replayed terminal = %v %q, want assistant %q", msgs[3].Role, messageString(msgs[3]), "all done")
	}
	if msgs[4].Role != llm.RoleUser || messageString(msgs[4]) != "again" {
		t.Errorf("last input = %v %q, want user %q", msgs[4].Role, messageString(msgs[4]), "again")
	}
}

// TestRunThreadRejectsNewInputWhileIncomplete pins the dangling-input rule:
// a run without a terminal record blocks new input and is continued only by
// ResumeThread, which re-asks the model without appending anything.
func TestRunThreadRejectsNewInputWhileIncomplete(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	stagedRunStarted(t, st, "t", "r-1", "crashed before reply")

	agent := persistTestAgent(t, st,
		MsgResponse(llm.AssistantMessage("late answer")),
		MsgResponse(llm.AssistantMessage("next answer")),
	)
	if _, err := agent.RunThread(ctx, "t", "new question"); !errors.Is(err, ErrRunIncomplete) {
		t.Fatalf("RunThread error = %v, want ErrRunIncomplete", err)
	}
	// The rejection must not have written anything.
	equalKinds(t, logKinds(t, st, "t"), store.KindRunStarted)

	res, err := agent.ResumeThread(ctx, "t")
	if err != nil {
		t.Fatalf("ResumeThread: %v", err)
	}
	if messageString(res.Message) != "late answer" {
		t.Errorf("resumed answer = %q", messageString(res.Message))
	}
	// The run is now closed; a new input is accepted and the provider never
	// sees two adjacent user turns: the original input is answered by the
	// resumed reply before the new input arrives.
	if _, err := agent.RunThread(ctx, "t", "next"); err != nil {
		t.Fatalf("RunThread after resume: %v", err)
	}
	msgs := agent.provider.(*MockProvider).LastMessages
	for i := 1; i < len(msgs); i++ {
		if msgs[i].Role == llm.RoleUser && msgs[i-1].Role == llm.RoleUser {
			t.Errorf("history has adjacent user messages at %d/%d", i-1, i)
		}
	}
}

// TestResumeThreadSynthesizesUnknownResults pins interrupted-round recovery:
// every unresolved declared call gets exactly one error tool result reporting
// the outcome as unknown, the synthesis is durably committed, and the run is
// closed after the resumed model reply.
func TestResumeThreadSynthesizesUnknownResults(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	head := stagedRunStarted(t, st, "t", "r-1", "go")
	head = stagedDeclaration(t, st, "t", "r-1", head, 0, toolCallMessage("c1", "c2"))

	agent := persistTestAgent(t, st, MsgResponse(llm.AssistantMessage("recovered")))
	res, err := agent.ResumeThread(ctx, "t")
	if err != nil {
		t.Fatalf("ResumeThread: %v", err)
	}
	if res.Message.Content == nil {
		t.Error("no resumed reply")
	}

	// The model saw the declaration followed by one result per call.
	msgs := agent.provider.(*MockProvider).LastMessages
	var declaredIdx, results int
	for _, m := range msgs {
		switch m.Role {
		case llm.RoleAssistant:
			declaredIdx++
		case llm.RoleTool:
			results++
		}
	}
	if declaredIdx != 1 || results != 2 {
		t.Fatalf("model saw %d declarations, %d results; want 1, 2", declaredIdx, results)
	}
	for _, m := range msgs {
		if m.Role != llm.RoleTool {
			continue
		}
		blocks := resultBlocks(m)
		if len(blocks) != 1 || !blocks[0].IsError {
			t.Errorf("synthesized result = %+v, want IsError", blocks)
		}
	}

	// The recovery commit is durable and the run is closed.
	equalKinds(t, logKinds(t, st, "t"),
		store.KindRunStarted, store.KindRoundDeclared, store.KindRoundCommitted, store.KindAgentEvent)
	if _, err := agent.ResumeThread(ctx, "t"); !errors.Is(err, ErrNothingToResume) {
		t.Errorf("second resume error = %v, want ErrNothingToResume", err)
	}
}

// TestTruncatedRunPersistsSkippedResults pins the maxIter case end to end:
// declared calls are known not to have executed, the durable Done does not
// re-carry the declared assistant message, and the replayed history is
// provider-valid — no trailing tool-call batch without results.
func TestTruncatedRunPersistsSkippedResults(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	reg := tool.NewRegistry()
	reg.MustRegister(&mockTool{
		info:   tool.ToolInfo{Name: "echo", Description: "echoes"},
		result: tool.NewTextResult("echoed"),
	})
	agent := New(NewMockProvider(MsgResponse(toolCallMessage("c1"))), reg,
		WithStore(st), WithMaxIter(1), WithLogger(discardLogger()))

	res, err := agent.RunThread(ctx, "t", "hi")
	if err != nil {
		t.Fatalf("RunThread: %v", err)
	}
	if !res.Truncated {
		t.Error("Truncated = false, want true")
	}
	equalKinds(t, logKinds(t, st, "t"),
		store.KindRunStarted, store.KindRoundDeclared, store.KindRoundCommitted, store.KindAgentEvent)

	records := logRecords(t, st, "t")
	var committed roundCommittedPayload
	if err := strictDecode(records[2].Payload, &committed); err != nil {
		t.Fatalf("committed payload: %v", err)
	}
	if len(committed.Results) != 1 {
		t.Fatalf("skipped results = %d, want 1", len(committed.Results))
	}
	blocks := resultBlocks(committed.Results[0])
	if len(blocks) != 1 || !blocks[0].IsError {
		t.Fatalf("skipped result = %+v, want IsError", blocks)
	}
	var done doneEventPayload
	if err := strictDecode(envelopeOf(t, records[3]).Payload, &done); err != nil {
		t.Fatalf("done payload: %v", err)
	}
	if !done.Truncated || len(done.Message.Content) != 0 {
		t.Errorf("truncated done = truncated %v, message content %d; want true, 0",
			done.Truncated, len(done.Message.Content))
	}

	// The replayed history is provider-valid: exactly user, declaration,
	// skipped result — no trailing unpaired assistant tool-call batch.
	agent2 := persistTestAgent(t, st, MsgResponse(llm.AssistantMessage("ok")))
	if _, err := agent2.RunThread(ctx, "t", "next"); err != nil {
		t.Fatalf("RunThread after truncation: %v", err)
	}
	msgs := agent2.provider.(*MockProvider).LastMessages
	want := []llm.Role{llm.RoleUser, llm.RoleAssistant, llm.RoleTool, llm.RoleUser}
	if len(msgs) != len(want) {
		t.Fatalf("replayed history = %d messages %v, want %d", len(msgs), roles(msgs), len(want))
	}
	for i := range want {
		if msgs[i].Role != want[i] {
			t.Errorf("replayed[%d] role = %v, want %v", i, msgs[i].Role, want[i])
		}
	}
	if last := resultBlocks(msgs[2]); len(last) != 1 || !last[0].IsError {
		t.Errorf("replayed result = %+v, want the skipped IsError result", last)
	}
}

func roles(msgs []llm.Message) []llm.Role {
	out := make([]llm.Role, len(msgs))
	for i, m := range msgs {
		out[i] = m.Role
	}
	return out
}
