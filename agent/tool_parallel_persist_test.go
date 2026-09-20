package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

func TestParallelStoreCommitsResultsInDeclarationOrder(t *testing.T) {
	firstStarted, secondStarted := make(chan struct{}), make(chan struct{})
	firstRelease, secondRelease := make(chan struct{}), make(chan struct{})
	reg := tool.NewRegistry()
	reg.MustRegister(&parallelTestTool{info: tool.ToolInfo{Name: "one"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		close(firstStarted)
		<-firstRelease
		return tool.NewTextResult("one"), nil
	}})
	reg.MustRegister(&parallelTestTool{info: tool.ToolInfo{Name: "two"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		close(secondStarted)
		<-secondRelease
		return tool.NewTextResult("two"), nil
	}})
	st := store.NewMemory()
	agent := New(parallelProvider(parallelToolCalls("one", "two")), reg,
		WithStore(st), WithToolConcurrency(2), WithLogger(discardLogger()))
	outcome := make(chan error, 1)
	go func() {
		_, err := agent.RunThread(context.Background(), "t", "go")
		outcome <- err
	}()
	waitParallelSignal(t, firstStarted)
	waitParallelSignal(t, secondStarted)
	close(secondRelease)
	close(firstRelease)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	select {
	case err := <-outcome:
		if err != nil {
			t.Fatalf("RunThread: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("parallel persisted run did not finish")
	}
	var committed roundCommittedPayload
	for _, record := range logRecords(t, st, "t") {
		if record.Kind == store.KindRoundCommitted {
			if err := strictDecode(record.Payload, &committed); err != nil {
				t.Fatalf("decode commit: %v", err)
			}
		}
	}
	if len(committed.Results) != 2 {
		t.Fatalf("committed result count = %d, want 2", len(committed.Results))
	}
	firstResult := resultBlocks(committed.Results[0])[0]
	secondResult := resultBlocks(committed.Results[1])[0]
	got := firstResult.ToolUseID + ":" + firstResult.Content + "/" + secondResult.ToolUseID + ":" + secondResult.Content
	if got != "c1:one/c2:two" {
		t.Errorf("round_committed results = %s, want c1:one/c2:two", got)
	}
}

func TestParallelInterruptedResultBatchResumesWithOrderedUnknowns(t *testing.T) {
	st := store.NewMemory()
	reg := tool.NewRegistry()
	for _, name := range []string{"one", "two"} {
		name := name
		reg.MustRegister(&parallelTestTool{info: tool.ToolInfo{Name: name}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
			return tool.NewTextResult(name), nil
		}})
	}
	agent := New(parallelProvider(parallelToolCalls("one", "two")), reg,
		WithStore(st), WithToolConcurrency(2), WithLogger(discardLogger()))
	seq, err := agent.RunThreadStream(context.Background(), "t", "go")
	if err != nil {
		t.Fatalf("RunThreadStream: %v", err)
	}
	for event, streamErr := range seq {
		if streamErr != nil {
			t.Fatalf("stream: %v", streamErr)
		}
		if _, ok := event.(ToolResultEvent); ok {
			break
		}
	}
	if hasKind(t, st, "t", store.KindRoundCommitted) {
		t.Fatal("consumer break committed an incomplete parallel round")
	}
	result, err := agent.ResumeThread(context.Background(), "t")
	if err != nil {
		t.Fatalf("ResumeThread: %v", err)
	}
	unknown := historyResultBlocks(result.History)
	if len(unknown) < 2 || unknown[0].ToolUseID != "c1" || unknown[1].ToolUseID != "c2" ||
		!unknown[0].IsError || !unknown[1].IsError ||
		!strings.Contains(unknown[0].Content, "outcome unknown") || !strings.Contains(unknown[1].Content, "outcome unknown") {
		t.Fatalf("recovered results = %#v, want ordered unknown c1/c2", unknown)
	}
}

func TestParallelSettledResultsCommitDespiteCancellation(t *testing.T) {
	st := store.NewMemory()
	reg := tool.NewRegistry()
	reg.MustRegister(&parallelTestTool{info: tool.ToolInfo{Name: "one"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult("one"), nil
	}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	seq, err := New(parallelProvider(parallelToolCalls("one")), reg,
		WithStore(st), WithToolConcurrency(2), WithLogger(discardLogger())).RunThreadStream(ctx, "settled", "go")
	if err != nil {
		t.Fatalf("RunThreadStream: %v", err)
	}
	var terminal error
	for event, streamErr := range seq {
		if streamErr != nil {
			terminal = streamErr
			break
		}
		if _, ok := event.(ToolResultEvent); ok {
			cancel()
		}
	}
	if !errors.Is(terminal, context.Canceled) || !hasKind(t, st, "settled", store.KindRoundCommitted) {
		t.Fatalf("settled cancellation error = %v, committed = %v", terminal, hasKind(t, st, "settled", store.KindRoundCommitted))
	}
}

func TestParallelCommitFailureLeavesOpenRound(t *testing.T) {
	reg := tool.NewRegistry()
	executed := atomic.Int32{}
	reg.MustRegister(&parallelTestTool{info: tool.ToolInfo{Name: "one"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		executed.Add(1)
		return tool.NewTextResult("one"), nil
	}})
	failed := &failAppendStore{Store: store.NewMemory(), failOnCall: 3}
	agent := New(parallelProvider(parallelToolCalls("one")), reg,
		WithStore(failed), WithToolConcurrency(2), WithLogger(discardLogger()))
	seq, err := agent.RunThreadStream(context.Background(), "failure", "go")
	if err != nil {
		t.Fatalf("RunThreadStream: %v", err)
	}
	seenResult := false
	for event, streamErr := range seq {
		if _, ok := event.(ToolResultEvent); ok {
			seenResult = true
		}
		if streamErr != nil {
			break
		}
	}
	if !seenResult || !hasKind(t, failed, "failure", store.KindRoundDeclared) || hasKind(t, failed, "failure", store.KindRoundCommitted) {
		t.Fatal("commit failure did not preserve delivered events and an open declaration")
	}

}

func TestParallelHistoryEntrypointsRemainNonPersistent(t *testing.T) {
	st := store.NewMemory()
	reg := tool.NewRegistry()
	reg.MustRegister(&parallelTestTool{info: tool.ToolInfo{Name: "one"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult("one"), nil
	}})
	agent := New(parallelProvider(parallelToolCalls("one")), reg,
		WithStore(st), WithToolConcurrency(2), WithLogger(discardLogger()))
	if _, err := agent.RunWithHistory(context.Background(), nil, "go"); err != nil {
		t.Fatalf("RunWithHistory: %v", err)
	}
	state, err := st.Latest(context.Background(), "unused")
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if state.Head != 0 {
		t.Errorf("history entrypoint wrote store head %d, want 0", state.Head)
	}
	streamAgent := New(parallelProvider(parallelToolCalls("one")), reg,
		WithStore(st), WithToolConcurrency(2), WithLogger(discardLogger()))
	seq, err := streamAgent.RunStreamWithHistory(context.Background(), nil, "go")
	if err != nil {
		t.Fatalf("RunStreamWithHistory: %v", err)
	}
	for _, streamErr := range seq {
		if streamErr != nil {
			t.Fatalf("RunStreamWithHistory stream: %v", streamErr)
		}
	}
	state, err = st.Latest(context.Background(), "unused")
	if err != nil {
		t.Fatalf("Latest after stream history entrypoint: %v", err)
	}
	if state.Head != 0 {
		t.Errorf("stream history entrypoint wrote store head %d, want 0", state.Head)
	}
	fallback := New(NewMockProvider(
		MsgResponse(llm.AssistantToolCallMessage(parallelToolCalls("one")[0])),
		MsgResponse(llm.AssistantMessage("done")),
	), reg, WithToolConcurrency(2), WithLogger(discardLogger()))
	if _, err := fallback.Run(context.Background(), "go"); err != nil {
		t.Fatalf("parallel non-streaming fallback Run: %v", err)
	}
}

func TestParallelBatchCompactsOnNextModelRound(t *testing.T) {
	st := store.NewMemory()
	reg := tool.NewRegistry()
	reg.MustRegister(&parallelTestTool{info: tool.ToolInfo{Name: "large"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult(strings.Repeat("x", 128)), nil
	}})
	agent := New(parallelProvider(parallelToolCalls("large")), reg,
		WithStore(st), WithToolConcurrency(2), WithContextWindowTokens(128),
		WithCompactor(compactKeepSystem{}), WithLogger(discardLogger()))
	if _, err := agent.RunThread(context.Background(), "compact", "go"); err != nil {
		t.Fatalf("RunThread: %v", err)
	}
	state, err := st.Latest(context.Background(), "compact")
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if state.Checkpoint == nil {
		t.Error("parallel batch did not compact before its next model round")
	}
}
