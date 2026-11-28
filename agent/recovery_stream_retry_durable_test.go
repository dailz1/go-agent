package agent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"syscall"
	"testing"
	"testing/synctest"

	"github.com/dailz1/go-agent/agenttest"
	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

func TestZeroDeliveryExhaustionResumesSameRunWithoutRepeatingTools(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		reg := tool.NewRegistry()
		executions := 0
		reg.MustRegister(mutatorTool("work", func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
			executions++
			return tool.NewTextResult("saved"), nil
		}))
		start := []llm.Message{llm.UserMessage("input")}
		call := &llm.ToolUseBlock{Type: "tool_use", ID: "old", Name: "work", Input: json.RawMessage(`{}`)}
		committed := append(deepCopyMessages(start), llm.AssistantToolCallMessage(call),
			llm.ToolResultMessage("old", tool.NewTextResult("saved")))
		exchanges := []agenttest.Exchange{
			recoveryExchange(start, nil, nil, &llm.ToolCallStartChunk{ID: "old", Name: "work"},
				&llm.ToolCallArgsChunk{Delta: `{}`}, &llm.DoneChunk{FinishReason: "tool_calls"}),
			recoveryExchange(committed, nil, syscall.ECONNRESET),
			recoveryExchange(committed, nil, syscall.ECONNRESET),
			recoveryExchange(committed, nil, nil, &llm.TextDeltaChunk{Text: "done"}, &llm.DoneChunk{FinishReason: "stop"}),
		}
		for i := range exchanges {
			exchanges[i].Request.Tools = reg.List()
		}
		p := agenttest.NewScriptedProvider(exchanges...)
		st := store.NewMemory()
		callbacks := 0
		var requests []RoundContextRequest
		var a *Agent
		a = New(p, reg, WithStore(st), WithLogger(discardLogger()),
			WithRoundContextProvider(func(_ context.Context, req RoundContextRequest) (string, error) {
				requests = append(requests, req)
				return "", nil
			}),
			WithRetryConfig(AgentRetryConfig{MaxRetries: 1, OnRetry: func(RetryInfo) {
				callbacks++
				v := recoveryView(t, a)
				if !v.runActive || v.open != nil || !reflect.DeepEqual(v.history, committed) {
					t.Fatalf("intermediate failure modified committed tool history: %+v", v)
				}
			}}))
		_, err := a.Run(t.Context(), "input", WithThread("recovery"))
		if !errors.Is(err, syscall.ECONNRESET) || callbacks != 1 || executions != 1 {
			t.Fatalf("error=%v callbacks=%d executions=%d", err, callbacks, executions)
		}
		before := recoveryView(t, a)
		if !before.runActive || before.open != nil || !reflect.DeepEqual(before.history, committed) {
			t.Fatalf("failed request committed an assistant or tool declaration: %+v", before)
		}
		if _, err := a.Run(t.Context(), "another", WithThread("recovery")); !errors.Is(err, ErrRunIncomplete) {
			t.Fatalf("incomplete run accepted another input: %v", err)
		}
		result, err := a.ResumeThread(t.Context(), "recovery")
		if err != nil || messageString(result.Message) != "done" || executions != 1 {
			t.Fatalf("resume=%+v error=%v executions=%d", result, err, executions)
		}
		after := recoveryView(t, a)
		if before.runID == "" || after.runID != before.runID || after.runActive ||
			!reflect.DeepEqual(after.history, append(committed, llm.AssistantMessage("done"))) {
			t.Fatalf("resume replaced run or lost results: before=%+v after=%+v", before, after)
		}
		wantRequests := []RoundContextRequest{
			{Round: 0, Attempt: 1, ThreadID: "recovery"},
			{Round: 1, Attempt: 1, ThreadID: "recovery"},
			{Round: 1, Attempt: 2, ThreadID: "recovery"},
			{Round: 1, Attempt: 1, ThreadID: "recovery"},
		}
		if !reflect.DeepEqual(requests, wantRequests) {
			t.Fatalf("request numbering = %+v", requests)
		}
		records, err := st.History(t.Context(), "recovery", 0)
		if err != nil {
			t.Fatal(err)
		}
		starts := 0
		for _, record := range records {
			if record.Kind == store.KindRunStarted {
				starts++
			}
		}
		if starts != 1 {
			t.Fatalf("run starts=%d, want 1", starts)
		}
		if err := p.Verify(); err != nil {
			t.Fatal(err)
		}
	})
}
