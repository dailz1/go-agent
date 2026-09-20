package agent

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

func TestUnifiedRunStreamEquivalence(t *testing.T) {
	// Given: equivalent streaming and non-streaming scripts for a two-round tool call.
	call := llm.ToolUseBlock{Type: "tool_use", ID: "call-1", Name: "echo", Input: json.RawMessage(`{"text":"hello"}`)}
	firstUsage := llm.Usage{InputTokens: 11, OutputTokens: 3, ReasoningTokens: 2}
	finalUsage := llm.Usage{InputTokens: 17, OutputTokens: 5, ReasoningTokens: 1}
	toolResponse := llm.Message{
		Role: llm.RoleAssistant,
		Content: []llm.ContentBlock{
			llm.TextBlock{Type: "text", Text: "I'll echo. "},
			call,
		},
	}
	finalResponse := llm.AssistantMessage("Echoed: hello")
	registry := tool.NewRegistry()
	registry.MustRegister(&mockTool{
		info:   tool.ToolInfo{Name: "echo", Description: "echoes input"},
		result: tool.NewTextResult("hello"),
	})
	streamingProvider := NewMockStreamingProvider([][]llm.Chunk{
		{
			llm.TextDeltaChunk{Text: "I'll "},
			llm.TextDeltaChunk{Text: "echo. "},
			llm.ToolCallStartChunk{Index: 0, ID: call.ID, Name: call.Name},
			llm.ToolCallArgsChunk{Index: 0, ID: call.ID, Delta: `{"text":`},
			llm.ToolCallArgsChunk{Index: 0, ID: call.ID, Delta: `"hello"}`},
			llm.DoneChunk{FinishReason: "tool_calls", Usage: &firstUsage},
		},
		{
			llm.TextDeltaChunk{Text: "Echoed: "},
			llm.TextDeltaChunk{Text: "hello"},
			llm.DoneChunk{FinishReason: "stop", Usage: &finalUsage},
		},
	})
	nonStreamingProvider := NewMockProvider(
		MsgWithUsageResponse(toolResponse, &firstUsage),
		MsgWithUsageResponse(finalResponse, &finalUsage),
	)
	streamingAgent := New(streamingProvider, registry, WithLogger(discardLogger()))
	nonStreamingAgent := New(nonStreamingProvider, registry, WithLogger(discardLogger()))

	// When: both public execution modes run the same scenario.
	seq, outerErr := streamingAgent.RunStream(context.Background(), "echo hello")
	events, streamErrs := collectEvents(t, seq, outerErr)
	runResult, runErr := nonStreamingAgent.Run(context.Background(), "echo hello")

	// Then: their completed result fields are equivalent.
	if len(streamErrs) != 0 {
		t.Fatalf("RunStream errors = %v, want none", streamErrs)
	}
	if runErr != nil {
		t.Fatalf("Run returned error: %v", runErr)
	}
	var streamResult DoneEvent
	foundDone := false
	for _, event := range events {
		if done, ok := event.(DoneEvent); ok {
			streamResult = done
			foundDone = true
		}
	}
	if !foundDone {
		t.Fatal("RunStream emitted no DoneEvent")
	}
	if got, want := messageText(&streamResult.Message), messageText(&runResult.Message); got != want {
		t.Errorf("Message text = %q, want %q", got, want)
	}
	if streamResult.ToolCalls != runResult.ToolCalls {
		t.Errorf("ToolCalls = %d, want %d", streamResult.ToolCalls, runResult.ToolCalls)
	}
	if streamResult.Truncated != runResult.Truncated {
		t.Errorf("Truncated = %t, want %t", streamResult.Truncated, runResult.Truncated)
	}
	if streamResult.Truncated {
		t.Error("Truncated = true, want false")
	}
	if streamResult.Usage != runResult.Usage {
		t.Errorf("Usage = %+v, want %+v", streamResult.Usage, runResult.Usage)
	}
	if streamResult.TotalUsage != runResult.TotalUsage {
		t.Errorf("TotalUsage = %+v, want %+v", streamResult.TotalUsage, runResult.TotalUsage)
	}
	if len(streamResult.Retries) != len(runResult.Retries) {
		t.Errorf("len(Retries) = %d, want %d", len(streamResult.Retries), len(runResult.Retries))
	}
}

func TestFallbackSynthesizesEvents(t *testing.T) {
	// Given: a non-streaming provider whose first response contains text and a tool call.
	args := json.RawMessage(`{"text":"hello"}`)
	toolResponse := llm.Message{
		Role: llm.RoleAssistant,
		Content: []llm.ContentBlock{
			llm.TextBlock{Type: "text", Text: "Using echo. "},
			llm.ToolUseBlock{Type: "tool_use", ID: "call-fallback", Name: "echo", Input: args},
		},
	}
	finalResponse := llm.AssistantMessage("Echo complete.")
	provider := NewMockProvider(MsgResponse(toolResponse), MsgResponse(finalResponse))
	registry := tool.NewRegistry()
	registry.MustRegister(&mockTool{
		info:   tool.ToolInfo{Name: "echo", Description: "echoes input"},
		result: tool.NewTextResult("hello"),
	})
	agent := New(provider, registry, WithLogger(discardLogger()))

	// When: RunStream falls back from ChatStream to Chat.
	seq, outerErr := agent.RunStream(context.Background(), "echo hello")
	events, streamErrs := collectEvents(t, seq, outerErr)

	// Then: the fallback synthesizes the complete observable event sequence.
	if len(streamErrs) != 0 {
		t.Fatalf("RunStream errors = %v, want none", streamErrs)
	}
	if len(events) != 5 {
		t.Fatalf("event count = %d, want 5", len(events))
	}
	firstText, ok := events[0].(TextDeltaEvent)
	if !ok {
		t.Fatalf("events[0] = %T, want TextDeltaEvent", events[0])
	}
	if firstText.Text != "Using echo. " {
		t.Errorf("first TextDeltaEvent.Text = %q, want %q", firstText.Text, "Using echo. ")
	}
	toolCall, ok := events[1].(ToolCallEvent)
	if !ok {
		t.Fatalf("events[1] = %T, want ToolCallEvent", events[1])
	}
	if toolCall.ID != "call-fallback" || toolCall.Name != "echo" || string(toolCall.Args) != string(args) {
		t.Errorf("ToolCallEvent = {ID:%q Name:%q Args:%s}, want {ID:%q Name:%q Args:%s}", toolCall.ID, toolCall.Name, toolCall.Args, "call-fallback", "echo", args)
	}
	toolResult, ok := events[2].(ToolResultEvent)
	if !ok {
		t.Fatalf("events[2] = %T, want ToolResultEvent", events[2])
	}
	if toolResult.ID != "call-fallback" || toolResult.Name != "echo" || toolResult.Result.Content != "hello" {
		t.Errorf("ToolResultEvent = {ID:%q Name:%q Result:%q}, want {ID:%q Name:%q Result:%q}", toolResult.ID, toolResult.Name, toolResult.Result.Content, "call-fallback", "echo", "hello")
	}
	finalText, ok := events[3].(TextDeltaEvent)
	if !ok {
		t.Fatalf("events[3] = %T, want TextDeltaEvent", events[3])
	}
	if finalText.Text != "Echo complete." {
		t.Errorf("final TextDeltaEvent.Text = %q, want %q", finalText.Text, "Echo complete.")
	}
	done, ok := events[4].(DoneEvent)
	if !ok {
		t.Fatalf("events[4] = %T, want DoneEvent", events[4])
	}
	if !reflect.DeepEqual(done.Message, finalResponse) {
		t.Errorf("DoneEvent.Message = %#v, want %#v", done.Message, finalResponse)
	}
}

func TestUnifiedRunStreamTerminalMaxIterEquivalence(t *testing.T) {
	calls := []llm.ToolUseBlock{
		{Type: "tool_use", ID: "c1", Name: "echo", Input: json.RawMessage(`{"n":1}`)},
		{Type: "tool_use", ID: "c2", Name: "echo", Input: json.RawMessage(`{"n":2}`)},
	}
	message := llm.AssistantToolCallMessage(calls...)
	registry := tool.NewRegistry()
	registry.MustRegister(&mockTool{info: tool.ToolInfo{Name: "echo"}, result: tool.NewTextResult("must not run")})
	streaming := New(
		NewMockStreamingProvider([][]llm.Chunk{parallelToolRound(calls)}),
		registry,
		WithMaxIter(1),
		WithLogger(discardLogger()),
	)
	fallback := New(
		NewMockProvider(MsgResponse(message)),
		registry,
		WithMaxIter(1),
		WithLogger(discardLogger()),
	)

	seq, err := streaming.RunStream(context.Background(), "go")
	events, errs := collectEvents(t, seq, err)
	if len(errs) != 0 {
		t.Fatalf("RunStream errors = %v", errs)
	}
	runResult, err := fallback.Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	done := terminalDone(t, events)
	if !reflect.DeepEqual(
		RunResult{
			Message: done.Message, History: done.History, ToolCalls: done.ToolCalls, Truncated: done.Truncated,
		},
		RunResult{
			Message: runResult.Message, History: runResult.History, ToolCalls: runResult.ToolCalls, Truncated: runResult.Truncated,
		},
	) {
		t.Errorf("Run and RunStream terminal results differ:\nstream=%#v\nrun=%#v", done, runResult)
	}
}

func TestRetryOnceCallback(t *testing.T) {
	// Given: two retryable failures followed by a successful fallback response.
	provider := NewRetryableMockProvider(
		&llm.APIError{StatusCode: 429, Body: "rate limited"},
		2,
		llm.AssistantMessage("recovered"),
	)
	var callbacks []RetryInfo
	agent := New(provider, tool.NewRegistry(),
		WithRetryConfig(AgentRetryConfig{
			MaxRetries: 2,
			BaseDelay:  time.Millisecond,
			MaxDelay:   time.Millisecond,
			OnRetry: func(info RetryInfo) {
				callbacks = append(callbacks, info)
			},
		}),
		WithLogger(discardLogger()),
	)

	// When: Run folds retry events into its result.
	result, err := agent.Run(context.Background(), "retry")

	// Then: each retry produces exactly one callback and one populated result entry.
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(callbacks) != len(result.Retries) {
		t.Fatalf("callback count = %d, want len(Retries) %d", len(callbacks), len(result.Retries))
	}
	if len(result.Retries) != 2 {
		t.Fatalf("len(Retries) = %d, want 2", len(result.Retries))
	}
	for i, retry := range result.Retries {
		if retry.Attempt == 0 {
			t.Errorf("Retries[%d].Attempt = 0, want populated", i)
		}
		if retry.Reason == "" {
			t.Errorf("Retries[%d].Reason is empty, want populated", i)
		}
		if callbacks[i].Attempt != retry.Attempt || callbacks[i].Reason != retry.Reason {
			t.Errorf("callback[%d] = {Attempt:%d Reason:%q}, want {Attempt:%d Reason:%q}", i, callbacks[i].Attempt, callbacks[i].Reason, retry.Attempt, retry.Reason)
		}
	}
}
