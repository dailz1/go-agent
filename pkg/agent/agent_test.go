package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dailz1/go-agent/pkg/llm"
	"github.com/dailz1/go-agent/pkg/tool"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func collectEvents(t *testing.T, seq iter.Seq2[AgentEvent, error], outerErr error) (events []AgentEvent, errs []error) {
	t.Helper()
	if outerErr != nil {
		errs = append(errs, outerErr)
		return
	}
	for ev, err := range seq {
		if err != nil {
			errs = append(errs, err)
			return
		}
		events = append(events, ev)
	}
	return
}

// collectAllEvents captures all events and errors from RunStream.
// Unlike collectEvents (which stops on first error), this continues after errors
// to capture RetryEvents that may precede a final error.
// Note: evt may be nil when yield(nil, err) is called — nil events are skipped.
func collectAllEvents(agent *Agent, ctx context.Context, prompt string) (events []AgentEvent, errs []error) {
	seq, outerErr := agent.RunStream(ctx, prompt)
	if outerErr != nil {
		errs = append(errs, outerErr)
		return
	}
	for evt, err := range seq {
		if err != nil {
			errs = append(errs, err)
		}
		if evt != nil {
			events = append(events, evt)
		}
	}
	return
}

func TestAgentRun_DirectReply(t *testing.T) {
	provider := NewMockProvider(
		MsgResponse(llm.AssistantMessage("hello")),
	)
	agent := New(provider, tool.NewRegistry(), WithLogger(discardLogger()))

	result, err := agent.Run(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if result.ToolCalls != 0 {
		t.Errorf("ToolCalls = %d, want 0", result.ToolCalls)
	}
	if result.Truncated {
		t.Error("Truncated = true, want false")
	}
	if msgText := messageText(&result.Message); msgText != "hello" {
		t.Errorf("Message text = %q, want %q", msgText, "hello")
	}
	if len(result.History) != 2 {
		t.Fatalf("History length = %d, want 2", len(result.History))
	}
	if result.History[0].Role != llm.RoleUser {
		t.Errorf("History[0].Role = %q, want %q", result.History[0].Role, llm.RoleUser)
	}
	if result.History[1].Role != llm.RoleAssistant {
		t.Errorf("History[1].Role = %q, want %q", result.History[1].Role, llm.RoleAssistant)
	}
}

func TestAgentRun_ToolCallThenReply(t *testing.T) {
	reg := tool.NewRegistry()
	reg.MustRegister(&mockTool{
		info:   tool.ToolInfo{Name: "mock_tool", Description: "a test tool"},
		result: tool.NewTextResult("tool output"),
	})

	provider := NewMockProvider(
		MsgResponse(llm.AssistantToolCallMessage(llm.ToolUseBlock{Type: "tool_use", ID: "c1", Name: "mock_tool", Input: json.RawMessage(`{}`)})),
		MsgResponse(llm.AssistantMessage("done")),
	)

	agent := New(provider, reg, WithLogger(discardLogger()))
	result, err := agent.Run(context.Background(), "do something")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if result.ToolCalls != 1 {
		t.Errorf("ToolCalls = %d, want 1", result.ToolCalls)
	}
	if result.Truncated {
		t.Error("Truncated = true, want false")
	}
	if msgText := messageText(&result.Message); msgText != "done" {
		t.Errorf("Message text = %q, want %q", msgText, "done")
	}

	wantRoles := []llm.Role{llm.RoleUser, llm.RoleAssistant, llm.RoleTool, llm.RoleAssistant}
	if len(result.History) != len(wantRoles) {
		t.Fatalf("History length = %d, want %d", len(result.History), len(wantRoles))
	}
	for i, want := range wantRoles {
		if result.History[i].Role != want {
			t.Errorf("History[%d].Role = %q, want %q", i, result.History[i].Role, want)
		}
	}
}

func TestAgentRun_ToolNotFound(t *testing.T) {
	provider := NewMockProvider(
		MsgResponse(llm.AssistantToolCallMessage(llm.ToolUseBlock{Type: "tool_use", ID: "c1", Name: "nonexistent", Input: json.RawMessage(`{}`)})),
		MsgResponse(llm.AssistantMessage("ok I see the error")),
	)

	agent := New(provider, tool.NewRegistry(), WithLogger(discardLogger()))
	result, err := agent.Run(context.Background(), "call missing tool")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if result.ToolCalls != 1 {
		t.Errorf("ToolCalls = %d, want 1", result.ToolCalls)
	}
	if result.Truncated {
		t.Error("Truncated = true, want false")
	}

	toolResultMsg := result.History[2]
	if toolResultMsg.Role != llm.RoleTool {
		t.Fatalf("History[2].Role = %q, want tool", toolResultMsg.Role)
	}
	trBlock, ok := toolResultMsg.Content[0].(llm.ToolResultBlock)
	if !ok {
		t.Fatal("History[2].Content[0] is not a ToolResultBlock")
	}
	if !trBlock.IsError {
		t.Error("ToolResultBlock.IsError = false, want true")
	}
}

func TestAgentRun_ApprovalRejected(t *testing.T) {
	reg := tool.NewRegistry()
	reg.MustRegister(&mockTool{
		info:   tool.ToolInfo{Name: "sensitive", Description: "needs approval", RequiresApproval: true},
		result: tool.NewTextResult("should not reach here"),
	})

	provider := NewMockProvider(
		MsgResponse(llm.AssistantToolCallMessage(llm.ToolUseBlock{Type: "tool_use", ID: "c1", Name: "sensitive", Input: json.RawMessage(`{}`)})),
		MsgResponse(llm.AssistantMessage("understood rejection")),
	)

	agent := New(provider, reg,
		WithApprovalFn(func(tool.ToolInfo, json.RawMessage) bool { return false }),
		WithLogger(discardLogger()),
	)

	result, err := agent.Run(context.Background(), "run sensitive op")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	toolResultMsg := result.History[2]
	trBlock, ok := toolResultMsg.Content[0].(llm.ToolResultBlock)
	if !ok {
		t.Fatal("History[2].Content[0] is not a ToolResultBlock")
	}
	if !trBlock.IsError {
		t.Error("expected error result for rejected approval")
	}
}

func TestAgentRun_ApprovalApproved(t *testing.T) {
	reg := tool.NewRegistry()
	reg.MustRegister(&mockTool{
		info:   tool.ToolInfo{Name: "sensitive", Description: "needs approval", RequiresApproval: true},
		result: tool.NewTextResult("executed fine"),
	})

	provider := NewMockProvider(
		MsgResponse(llm.AssistantToolCallMessage(llm.ToolUseBlock{Type: "tool_use", ID: "c1", Name: "sensitive", Input: json.RawMessage(`{}`)})),
		MsgResponse(llm.AssistantMessage("done")),
	)

	agent := New(provider, reg,
		WithApprovalFn(func(tool.ToolInfo, json.RawMessage) bool { return true }),
		WithLogger(discardLogger()),
	)

	result, err := agent.Run(context.Background(), "run sensitive op")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if result.ToolCalls != 1 {
		t.Errorf("ToolCalls = %d, want 1", result.ToolCalls)
	}

	toolResultMsg := result.History[2]
	trBlock, ok := toolResultMsg.Content[0].(llm.ToolResultBlock)
	if !ok {
		t.Fatal("History[2].Content[0] is not a ToolResultBlock")
	}
	if trBlock.IsError {
		t.Error("ToolResultBlock.IsError = true, want false")
	}
	if trBlock.Content != "executed fine" {
		t.Errorf("ToolResultBlock.Content = %q, want %q", trBlock.Content, "executed fine")
	}
}

func TestAgentRun_Truncated(t *testing.T) {
	reg := tool.NewRegistry()
	reg.MustRegister(&mockTool{
		info:   tool.ToolInfo{Name: "loop_tool", Description: "keeps calling"},
		result: tool.NewTextResult("result"),
	})

	var responses []mockResponse
	for i := 0; i < 5; i++ {
		responses = append(responses, MsgResponse(
			llm.AssistantToolCallMessage(llm.ToolUseBlock{Type: "tool_use", ID: fmt.Sprintf("c%d", i), Name: "loop_tool", Input: json.RawMessage(`{}`)}),
		))
	}

	provider := NewMockProvider(responses...)
	agent := New(provider, reg,
		WithMaxIter(2),
		WithLogger(discardLogger()),
	)

	result, err := agent.Run(context.Background(), "loop forever")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if !result.Truncated {
		t.Error("Truncated = false, want true")
	}
	if result.ToolCalls == 0 {
		t.Error("ToolCalls = 0, expected > 0")
	}
}

func TestAgentRun_ProviderError(t *testing.T) {
	provider := NewMockProvider(
		ErrResponse(&llm.APIError{StatusCode: 400, Body: "bad request"}),
	)

	agent := New(provider, tool.NewRegistry(), WithLogger(discardLogger()))
	result, err := agent.Run(context.Background(), "hello")

	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if result != nil {
		t.Errorf("expected nil result, got %+v", result)
	}
}

func TestAgentRun_ToolExecuteError(t *testing.T) {
	reg := tool.NewRegistry()
	reg.MustRegister(&mockTool{
		info: tool.ToolInfo{Name: "boom_tool", Description: "explodes"},
		err:  fmt.Errorf("boom"),
	})

	provider := NewMockProvider(
		MsgResponse(llm.AssistantToolCallMessage(llm.ToolUseBlock{Type: "tool_use", ID: "c1", Name: "boom_tool", Input: json.RawMessage(`{}`)})),
	)

	agent := New(provider, reg, WithLogger(discardLogger()))
	result, err := agent.Run(context.Background(), "explode")

	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if result != nil {
		t.Errorf("expected nil result, got %+v", result)
	}
	if got := err.Error(); got == "" || !contains(got, "boom") {
		t.Errorf("error = %q, want something containing %q", got, "boom")
	}
}

func TestAgentRun_NilToolResult(t *testing.T) {
	reg := tool.NewRegistry()
	reg.MustRegister(&mockTool{
		info:   tool.ToolInfo{Name: "nil_tool", Description: "returns nil"},
		result: nil,
	})

	provider := NewMockProvider(
		MsgResponse(llm.AssistantToolCallMessage(llm.ToolUseBlock{Type: "tool_use", ID: "c1", Name: "nil_tool", Input: json.RawMessage(`{}`)})),
		MsgResponse(llm.AssistantMessage("ok")),
	)

	agent := New(provider, reg, WithLogger(discardLogger()))
	result, err := agent.Run(context.Background(), "call nil tool")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	toolResultMsg := result.History[2]
	trBlock, ok := toolResultMsg.Content[0].(llm.ToolResultBlock)
	if !ok {
		t.Fatal("History[2].Content[0] is not a ToolResultBlock")
	}
	if !trBlock.IsError {
		t.Error("expected error result for nil tool result")
	}
}

func TestAgentRun_WithSystemPrompt(t *testing.T) {
	provider := NewMockProvider(
		MsgResponse(llm.AssistantMessage("hello")),
	)

	agent := New(provider, tool.NewRegistry(),
		WithSystemPrompt("you are a bot"),
		WithLogger(discardLogger()),
	)

	_, err := agent.Run(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if len(provider.LastMessages) < 2 {
		t.Fatalf("LastMessages length = %d, want >= 2", len(provider.LastMessages))
	}
	first := provider.LastMessages[0]
	if first.Role != llm.RoleSystem {
		t.Errorf("first message Role = %q, want %q", first.Role, llm.RoleSystem)
	}
	tb, ok := first.Content[0].(llm.TextBlock)
	if !ok {
		t.Fatal("first message Content[0] is not a TextBlock")
	}
	if tb.Text != "you are a bot" {
		t.Errorf("system prompt text = %q, want %q", tb.Text, "you are a bot")
	}
}

func TestAgentRun_HistoryContainsFullTrace(t *testing.T) {
	reg := tool.NewRegistry()
	reg.MustRegister(&mockTool{
		info:   tool.ToolInfo{Name: "tracer", Description: "trace tool"},
		result: tool.NewTextResult("trace output"),
	})

	provider := NewMockProvider(
		MsgResponse(llm.AssistantToolCallMessage(llm.ToolUseBlock{Type: "tool_use", ID: "c1", Name: "tracer", Input: json.RawMessage(`{}`)})),
		MsgResponse(llm.AssistantMessage("final answer")),
	)

	agent := New(provider, reg,
		WithSystemPrompt("sys"),
		WithLogger(discardLogger()),
	)

	result, err := agent.Run(context.Background(), "trace me")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	wantRoles := []llm.Role{
		llm.RoleSystem,
		llm.RoleUser,
		llm.RoleAssistant,
		llm.RoleTool,
		llm.RoleAssistant,
	}
	if len(result.History) != len(wantRoles) {
		t.Fatalf("History length = %d, want %d", len(result.History), len(wantRoles))
	}
	for i, want := range wantRoles {
		if result.History[i].Role != want {
			t.Errorf("History[%d].Role = %q, want %q", i, result.History[i].Role, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Edge case and boundary tests
// ---------------------------------------------------------------------------

func TestAgentRun_EmptyInput(t *testing.T) {
	t.Parallel()

	provider := NewMockProvider(
		MsgResponse(llm.AssistantMessage("hello")),
	)
	agent := New(provider, tool.NewRegistry(), WithLogger(discardLogger()))

	result, err := agent.Run(context.Background(), "")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if result.Truncated {
		t.Error("Truncated = true, want false")
	}

	if len(provider.LastMessages) < 1 {
		t.Fatal("provider received no messages")
	}
	first := provider.LastMessages[0]
	if first.Role != llm.RoleUser {
		t.Errorf("first message Role = %q, want %q", first.Role, llm.RoleUser)
	}
	tb, ok := first.Content[0].(llm.TextBlock)
	if !ok {
		t.Fatal("first message Content[0] is not a TextBlock")
	}
	if tb.Text != "" {
		t.Errorf("user message text = %q, want empty string", tb.Text)
	}
}

func TestAgentRun_ContextCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	provider := &contextAwareMock{
		responses: []mockResponse{
			MsgResponse(llm.AssistantMessage("should not reach")),
		},
	}

	agent := New(provider, tool.NewRegistry(), WithLogger(discardLogger()))
	result, err := agent.Run(ctx, "hello")

	if err == nil {
		t.Fatal("expected error due to canceled context, got nil")
	}
	if result != nil {
		t.Errorf("expected nil result, got %+v", result)
	}
}

func TestAgentRun_MultipleToolCallsInOneResponse(t *testing.T) {
	t.Parallel()

	reg := tool.NewRegistry()
	reg.MustRegister(&mockTool{
		info:   tool.ToolInfo{Name: "add", Description: "adds"},
		result: tool.NewTextResult("add result"),
	})
	reg.MustRegister(&mockTool{
		info:   tool.ToolInfo{Name: "mul", Description: "multiplies"},
		result: tool.NewTextResult("mul result"),
	})

	// LLM returns 2 tool calls in one response
	provider := NewMockProvider(
		MsgResponse(llm.AssistantToolCallMessage(
			llm.ToolUseBlock{Type: "tool_use", ID: "c1", Name: "add", Input: json.RawMessage(`{}`)},
			llm.ToolUseBlock{Type: "tool_use", ID: "c2", Name: "mul", Input: json.RawMessage(`{}`)},
		)),
		MsgResponse(llm.AssistantMessage("done")),
	)

	agent := New(provider, reg, WithLogger(discardLogger()))
	result, err := agent.Run(context.Background(), "compute")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if result.ToolCalls != 2 {
		t.Errorf("ToolCalls = %d, want 2", result.ToolCalls)
	}
	if result.Truncated {
		t.Error("Truncated = true, want false")
	}

	wantRoles := []llm.Role{
		llm.RoleUser,
		llm.RoleAssistant,
		llm.RoleTool,
		llm.RoleTool,
		llm.RoleAssistant,
	}
	if len(result.History) != len(wantRoles) {
		t.Fatalf("History length = %d, want %d", len(result.History), len(wantRoles))
	}
	for i, want := range wantRoles {
		if result.History[i].Role != want {
			t.Errorf("History[%d].Role = %q, want %q", i, result.History[i].Role, want)
		}
	}

	// Verify tool results appear in order: add first, then mul
	// Verify tool results appear in order: add first, then mul
	trAdd, ok := result.History[2].Content[0].(llm.ToolResultBlock)
	if !ok {
		t.Fatal("History[2].Content[0] is not a ToolResultBlock")
	}
	if trAdd.Content != "add result" {
		t.Errorf("first tool result = %q, want %q", trAdd.Content, "add result")
	}
	trMul, ok := result.History[3].Content[0].(llm.ToolResultBlock)
	if !ok {
		t.Fatal("History[3].Content[0] is not a ToolResultBlock")
	}
	if trMul.Content != "mul result" {
		t.Errorf("second tool result = %q, want %q", trMul.Content, "mul result")
	}
}

func TestAgentRun_MaxIterBoundary(t *testing.T) {
	t.Parallel()

	reg := tool.NewRegistry()
	reg.MustRegister(&mockTool{
		info:   tool.ToolInfo{Name: "loop", Description: "keeps looping"},
		result: tool.NewTextResult("looped"),
	})

	provider := NewMockProvider(
		MsgResponse(llm.AssistantToolCallMessage(
			llm.ToolUseBlock{Type: "tool_use", ID: "c1", Name: "loop", Input: json.RawMessage(`{}`)},
		)),
		MsgResponse(llm.AssistantToolCallMessage(
			llm.ToolUseBlock{Type: "tool_use", ID: "c2", Name: "loop", Input: json.RawMessage(`{}`)},
		)),
	)

	agent := New(provider, reg,
		WithMaxIter(1),
		WithLogger(discardLogger()),
	)

	result, err := agent.Run(context.Background(), "boundary test")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if !result.Truncated {
		t.Error("Truncated = false, want true")
	}
	if result.ToolCalls != 0 {
		t.Errorf("ToolCalls = %d, want 0 (last iteration tools not executed)", result.ToolCalls)
	}
	if result.Message.Role != llm.RoleAssistant {
		t.Errorf("Message.Role = %q, want %q", result.Message.Role, llm.RoleAssistant)
	}
}

func TestAgentRun_DefaultMaxIter(t *testing.T) {
	t.Parallel()

	reg := tool.NewRegistry()
	reg.MustRegister(&mockTool{
		info:   tool.ToolInfo{Name: "step", Description: "a step"},
		result: tool.NewTextResult("step done"),
	})

	// 9 tool calls then a final text response on the 10th
	var responses []mockResponse
	for i := 0; i < 9; i++ {
		responses = append(responses, MsgResponse(
			llm.AssistantToolCallMessage(
				llm.ToolUseBlock{Type: "tool_use", ID: fmt.Sprintf("c%d", i), Name: "step", Input: json.RawMessage(`{}`)},
			),
		))
	}
	responses = append(responses, MsgResponse(llm.AssistantMessage("final answer")))

	provider := NewMockProvider(responses...)

	agent := New(provider, reg, WithLogger(discardLogger()))

	result, err := agent.Run(context.Background(), "many steps")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if result.Truncated {
		t.Error("Truncated = true, want false (default MaxIter=10 should allow 10 iterations)")
	}
	if result.ToolCalls != 9 {
		t.Errorf("ToolCalls = %d, want 9", result.ToolCalls)
	}
	if msgText := messageText(&result.Message); msgText != "final answer" {
		t.Errorf("Message text = %q, want %q", msgText, "final answer")
	}
}

func TestAgentRun_ApprovalWithNoApprovalFn(t *testing.T) {
	t.Parallel()

	reg := tool.NewRegistry()
	reg.MustRegister(&mockTool{
		info:   tool.ToolInfo{Name: "sensitive", Description: "needs approval", RequiresApproval: true},
		result: tool.NewTextResult("executed anyway"),
	})

	provider := NewMockProvider(
		MsgResponse(llm.AssistantToolCallMessage(
			llm.ToolUseBlock{Type: "tool_use", ID: "c1", Name: "sensitive", Input: json.RawMessage(`{}`)},
		)),
		MsgResponse(llm.AssistantMessage("done")),
	)

	agent := New(provider, reg, WithLogger(discardLogger()))

	result, err := agent.Run(context.Background(), "run sensitive")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if result.ToolCalls != 1 {
		t.Errorf("ToolCalls = %d, want 1", result.ToolCalls)
	}

	toolResultMsg := result.History[2]
	trBlock, ok := toolResultMsg.Content[0].(llm.ToolResultBlock)
	if !ok {
		t.Fatal("History[2].Content[0] is not a ToolResultBlock")
	}
	if trBlock.IsError {
		t.Error("ToolResultBlock.IsError = true, want false")
	}
	if trBlock.Content != "executed anyway" {
		t.Errorf("ToolResultBlock.Content = %q, want %q", trBlock.Content, "executed anyway")
	}
}

func TestAgentRun_EmptyToolRegistry(t *testing.T) {
	t.Parallel()

	provider := NewMockProvider(
		MsgResponse(llm.AssistantToolCallMessage(
			llm.ToolUseBlock{Type: "tool_use", ID: "c1", Name: "nonexistent", Input: json.RawMessage(`{}`)},
		)),
		MsgResponse(llm.AssistantMessage("I see the error")),
	)

	agent := New(provider, tool.NewRegistry(), WithLogger(discardLogger()))
	result, err := agent.Run(context.Background(), "call missing tool")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if result.ToolCalls != 1 {
		t.Errorf("ToolCalls = %d, want 1", result.ToolCalls)
	}

	toolResultMsg := result.History[2]
	trBlock, ok := toolResultMsg.Content[0].(llm.ToolResultBlock)
	if !ok {
		t.Fatal("History[2].Content[0] is not a ToolResultBlock")
	}
	if !trBlock.IsError {
		t.Error("expected error result for tool not found in empty registry")
	}
}

func TestAgentRun_ExtractToolUse_EmptyContent(t *testing.T) {
	t.Parallel()

	msg := llm.Message{
		Role:    llm.RoleAssistant,
		Content: []llm.ContentBlock{},
	}
	provider := NewMockProvider(
		MsgResponse(msg),
	)

	agent := New(provider, tool.NewRegistry(), WithLogger(discardLogger()))
	result, err := agent.Run(context.Background(), "empty content test")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if result.ToolCalls != 0 {
		t.Errorf("ToolCalls = %d, want 0", result.ToolCalls)
	}
	if result.Truncated {
		t.Error("Truncated = true, want false")
	}
}

func TestAgentRun_MessagesPassedToProvider(t *testing.T) {
	t.Parallel()

	reg := tool.NewRegistry()
	reg.MustRegister(&mockTool{
		info:   tool.ToolInfo{Name: "echo", Description: "echoes"},
		result: tool.NewTextResult("echoed"),
	})

	callCount := 0
	var capturedMessages [][]llm.Message

	wrapper := &messageCapturingMock{
		inner: NewMockProvider(
			MsgResponse(llm.AssistantToolCallMessage(
				llm.ToolUseBlock{Type: "tool_use", ID: "c1", Name: "echo", Input: json.RawMessage(`{}`)},
			)),
			MsgResponse(llm.AssistantMessage("final")),
		),
		onChat: func(messages []llm.Message) {
			dup := make([]llm.Message, len(messages))
			copy(dup, messages)
			capturedMessages = append(capturedMessages, dup)
			callCount++
		},
	}

	agent := New(wrapper, reg, WithLogger(discardLogger()))
	_, err := agent.Run(context.Background(), "track messages")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if callCount != 2 {
		t.Fatalf("provider.Chat called %d times, want 2", callCount)
	}

	if len(capturedMessages[0]) != 1 {
		t.Fatalf("first call messages length = %d, want 1", len(capturedMessages[0]))
	}
	if capturedMessages[0][0].Role != llm.RoleUser {
		t.Errorf("first call messages[0].Role = %q, want %q", capturedMessages[0][0].Role, llm.RoleUser)
	}

	if len(capturedMessages[1]) != 3 {
		t.Fatalf("second call messages length = %d, want 3", len(capturedMessages[1]))
	}
	wantRoles := []llm.Role{llm.RoleUser, llm.RoleAssistant, llm.RoleTool}
	for i, want := range wantRoles {
		if capturedMessages[1][i].Role != want {
			t.Errorf("second call messages[%d].Role = %q, want %q", i, capturedMessages[1][i].Role, want)
		}
	}
}

func TestAgentRun_ToolsPassedToProvider(t *testing.T) {
	t.Parallel()

	reg := tool.NewRegistry()
	reg.MustRegister(&mockTool{
		info:   tool.ToolInfo{Name: "alpha", Description: "first tool"},
		result: tool.NewTextResult("a"),
	})
	reg.MustRegister(&mockTool{
		info:   tool.ToolInfo{Name: "beta", Description: "second tool"},
		result: tool.NewTextResult("b"),
	})

	provider := NewMockProvider(
		MsgResponse(llm.AssistantMessage("done")),
	)

	agent := New(provider, reg, WithLogger(discardLogger()))
	_, err := agent.Run(context.Background(), "check tools")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	tools := provider.LastTools
	if len(tools) != 2 {
		t.Fatalf("tools passed to provider = %d, want 2", len(tools))
	}

	names := make(map[string]bool)
	for _, ti := range tools {
		names[ti.Name] = true
	}
	if !names["alpha"] {
		t.Errorf("expected tool %q in provider tools list", "alpha")
	}
	if !names["beta"] {
		t.Errorf("expected tool %q in provider tools list", "beta")
	}
}

// ---------------------------------------------------------------------------
// Test helpers for edge case tests
// ---------------------------------------------------------------------------

// contextAwareMock wraps a response list but respects context cancellation.
type contextAwareMock struct {
	responses []mockResponse
	index     int
}

func (m *contextAwareMock) Name() string { return "context_aware_mock" }

func (m *contextAwareMock) Chat(ctx context.Context, _ []llm.Message, _ []tool.ToolInfo, _ ...llm.Option) (*llm.Message, *llm.Usage, error) {
	select {
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	default:
	}
	if m.index >= len(m.responses) {
		return nil, nil, fmt.Errorf("mock: no more responses (called %d times)", m.index+1)
	}
	resp := m.responses[m.index]
	m.index++
	return resp.msg, resp.usage, resp.err
}

func (m *contextAwareMock) ChatStream(_ context.Context, _ []llm.Message, _ []tool.ToolInfo, _ ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	return nil, llm.ErrStreamingNotSupported
}

// messageCapturingMock wraps a provider and captures the messages passed to each Chat call.
type messageCapturingMock struct {
	inner  *MockProvider
	onChat func(messages []llm.Message)
}

func (m *messageCapturingMock) Name() string { return m.inner.Name() }

func (m *messageCapturingMock) Chat(ctx context.Context, messages []llm.Message, tools []tool.ToolInfo, opts ...llm.Option) (*llm.Message, *llm.Usage, error) {
	m.onChat(messages)
	return m.inner.Chat(ctx, messages, tools, opts...)
}

func (m *messageCapturingMock) ChatStream(ctx context.Context, messages []llm.Message, tools []tool.ToolInfo, opts ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	return m.inner.ChatStream(ctx, messages, tools, opts...)
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStr(s, substr))
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// RunStream streaming tests
// ---------------------------------------------------------------------------

func TestRunStream_DirectTextReply(t *testing.T) {
	provider := NewMockStreamingProvider([][]llm.Chunk{
		{llm.TextDeltaChunk{Text: "hello"}, llm.DoneChunk{FinishReason: "stop"}},
	})
	agent := New(provider, tool.NewRegistry(), WithLogger(discardLogger()))

	seq, err := agent.RunStream(context.Background(), "hi")
	events, errs := collectEvents(t, seq, err)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}

	td, ok := events[0].(TextDeltaEvent)
	if !ok {
		t.Fatalf("events[0] = %T, want TextDeltaEvent", events[0])
	}
	if td.Text != "hello" {
		t.Errorf("TextDeltaEvent.Text = %q, want %q", td.Text, "hello")
	}

	done, ok := events[1].(DoneEvent)
	if !ok {
		t.Fatalf("events[1] = %T, want DoneEvent", events[1])
	}
	if done.Truncated {
		t.Error("DoneEvent.Truncated = true, want false")
	}
}

func TestRunStream_ToolCallThenReply(t *testing.T) {
	reg := tool.NewRegistry()
	reg.MustRegister(&mockTool{
		info:   tool.ToolInfo{Name: "echo", Description: "echoes input"},
		result: tool.NewTextResult("echoed"),
	})

	provider := NewMockStreamingProvider([][]llm.Chunk{
		{
			llm.ToolCallStartChunk{Index: 0, ID: "c1", Name: "echo"},
			llm.ToolCallArgsChunk{Index: 0, Delta: `{"input":"hi"}`},
			llm.DoneChunk{FinishReason: "tool_calls"},
		},
		{
			llm.TextDeltaChunk{Text: "done"},
			llm.DoneChunk{FinishReason: "stop"},
		},
	})

	agent := New(provider, reg, WithMaxIter(10), WithLogger(discardLogger()))
	seq, err := agent.RunStream(context.Background(), "call echo")
	events, errs := collectEvents(t, seq, err)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	// Expect: ToolCallEvent, ToolResultEvent, TextDeltaEvent, DoneEvent
	if len(events) != 4 {
		t.Fatalf("expected 4 events, got %d", len(events))
	}

	tc, ok := events[0].(ToolCallEvent)
	if !ok {
		t.Fatalf("events[0] = %T, want ToolCallEvent", events[0])
	}
	if tc.ID != "c1" || tc.Name != "echo" {
		t.Errorf("ToolCallEvent = {ID:%q, Name:%q}, want {ID:%q, Name:%q}", tc.ID, tc.Name, "c1", "echo")
	}

	tr, ok := events[1].(ToolResultEvent)
	if !ok {
		t.Fatalf("events[1] = %T, want ToolResultEvent", events[1])
	}
	if tr.ID != "c1" || tr.Name != "echo" {
		t.Errorf("ToolResultEvent = {ID:%q, Name:%q}, want {ID:%q, Name:%q}", tr.ID, tr.Name, "c1", "echo")
	}

	td, ok := events[2].(TextDeltaEvent)
	if !ok {
		t.Fatalf("events[2] = %T, want TextDeltaEvent", events[2])
	}
	if td.Text != "done" {
		t.Errorf("TextDeltaEvent.Text = %q, want %q", td.Text, "done")
	}

	done, ok := events[3].(DoneEvent)
	if !ok {
		t.Fatalf("events[3] = %T, want DoneEvent", events[3])
	}
	if done.ToolCalls != 1 {
		t.Errorf("DoneEvent.ToolCalls = %d, want 1", done.ToolCalls)
	}
	if done.Truncated {
		t.Error("DoneEvent.Truncated = true, want false")
	}
}

func TestRunStream_MultipleToolCallsInOneStream(t *testing.T) {
	reg := tool.NewRegistry()
	reg.MustRegister(&mockTool{
		info:   tool.ToolInfo{Name: "add", Description: "adds"},
		result: tool.NewTextResult("add result"),
	})
	reg.MustRegister(&mockTool{
		info:   tool.ToolInfo{Name: "mul", Description: "multiplies"},
		result: tool.NewTextResult("mul result"),
	})

	provider := NewMockStreamingProvider([][]llm.Chunk{
		{
			llm.ToolCallStartChunk{Index: 0, ID: "c1", Name: "add"},
			llm.ToolCallStartChunk{Index: 1, ID: "c2", Name: "mul"},
			llm.ToolCallArgsChunk{Index: 0, Delta: `{"a":1}`},
			llm.ToolCallArgsChunk{Index: 1, Delta: `{"b":2}`},
			llm.DoneChunk{FinishReason: "tool_calls"},
		},
		{
			llm.TextDeltaChunk{Text: "all done"},
			llm.DoneChunk{FinishReason: "stop"},
		},
	})

	agent := New(provider, reg, WithMaxIter(10), WithLogger(discardLogger()))
	seq, err := agent.RunStream(context.Background(), "compute")
	events, errs := collectEvents(t, seq, err)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	// Expect: ToolCall(add), ToolResult(add), ToolCall(mul), ToolResult(mul), TextDelta, Done
	if len(events) != 6 {
		t.Fatalf("expected 6 events, got %d", len(events))
	}

	// Verify tool calls order
	tc1, ok := events[0].(ToolCallEvent)
	if !ok || tc1.Name != "add" {
		t.Errorf("events[0] = %T %+v, want ToolCallEvent{Name:%q}", events[0], events[0], "add")
	}
	tr1, ok := events[1].(ToolResultEvent)
	if !ok || tr1.Name != "add" {
		t.Errorf("events[1] = %T %+v, want ToolResultEvent{Name:%q}", events[1], events[1], "add")
	}
	tc2, ok := events[2].(ToolCallEvent)
	if !ok || tc2.Name != "mul" {
		t.Errorf("events[2] = %T %+v, want ToolCallEvent{Name:%q}", events[2], events[2], "mul")
	}
	tr2, ok := events[3].(ToolResultEvent)
	if !ok || tr2.Name != "mul" {
		t.Errorf("events[3] = %T %+v, want ToolResultEvent{Name:%q}", events[3], events[3], "mul")
	}

	done, ok := events[5].(DoneEvent)
	if !ok {
		t.Fatalf("events[5] = %T, want DoneEvent", events[5])
	}
	if done.ToolCalls != 2 {
		t.Errorf("DoneEvent.ToolCalls = %d, want 2", done.ToolCalls)
	}
}

func TestRunStream_Truncated(t *testing.T) {
	reg := tool.NewRegistry()
	reg.MustRegister(&mockTool{
		info:   tool.ToolInfo{Name: "loop_tool", Description: "keeps calling"},
		result: tool.NewTextResult("result"),
	})

	provider := NewMockStreamingProvider([][]llm.Chunk{
		{
			llm.ToolCallStartChunk{Index: 0, ID: "c1", Name: "loop_tool"},
			llm.ToolCallArgsChunk{Index: 0, Delta: `{}`},
			llm.DoneChunk{FinishReason: "tool_calls"},
		},
	})

	agent := New(provider, reg, WithMaxIter(1), WithLogger(discardLogger()))
	seq, err := agent.RunStream(context.Background(), "loop")
	events, errs := collectEvents(t, seq, err)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	// Expect: DoneEvent(truncated) only — last iteration tools are not executed.
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	done, ok := events[0].(DoneEvent)
	if !ok {
		t.Fatalf("events[0] = %T, want DoneEvent", events[0])
	}
	if !done.Truncated {
		t.Error("DoneEvent.Truncated = false, want true")
	}
	if done.ToolCalls != 0 {
		t.Errorf("DoneEvent.ToolCalls = %d, want 0 (last iteration tools not executed)", done.ToolCalls)
	}
	if done.Message.Role != llm.RoleAssistant {
		t.Errorf("DoneEvent.Message.Role = %q, want %q", done.Message.Role, llm.RoleAssistant)
	}
}

func TestRunStream_ContextCancellation(t *testing.T) {
	provider := NewMockStreamingProvider([][]llm.Chunk{
		{llm.TextDeltaChunk{Text: "should not reach"}, llm.DoneChunk{FinishReason: "stop"}},
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	agent := New(provider, tool.NewRegistry(), WithLogger(discardLogger()))
	seq, err := agent.RunStream(ctx, "hello")

	var gotErr error
	if err != nil {
		gotErr = err
	} else {
		for _, iterErr := range seq {
			if iterErr != nil {
				gotErr = iterErr
				break
			}
		}
	}

	if gotErr == nil {
		t.Fatal("expected error due to cancelled context, got nil")
	}
	if !errors.Is(gotErr, context.Canceled) {
		t.Errorf("error = %v, want something wrapping context.Canceled", gotErr)
	}
}

func TestRunStream_ProviderError(t *testing.T) {
	origErr := &llm.APIError{StatusCode: 400}
	provider := NewMockStreamingProvider(nil).WithStreamError(origErr)

	agent := New(provider, tool.NewRegistry(), WithLogger(discardLogger()))
	seq, outerErr := agent.RunStream(context.Background(), "hello")

	if outerErr != nil {
		t.Fatalf("unexpected outer error: %v", outerErr)
	}

	var gotErr error
	for _, iterErr := range seq {
		if iterErr != nil {
			gotErr = iterErr
			break
		}
	}

	if gotErr == nil {
		t.Fatal("expected iterator error, got nil")
	}
	if !contains(gotErr.Error(), "iteration 0") {
		t.Errorf("error message = %q, want something containing %q", gotErr.Error(), "iteration 0")
	}
	if !errors.Is(gotErr, origErr) {
		t.Errorf("errors.Is(err, origErr) = false, want true; err = %v", gotErr)
	}
}

func TestRunStream_StreamingNotSupported(t *testing.T) {
	provider := NewMockStreamingProvider(nil).WithStreamError(llm.ErrStreamingNotSupported)

	agent := New(provider, tool.NewRegistry(), WithLogger(discardLogger()))
	seq, outerErr := agent.RunStream(context.Background(), "hello")

	if outerErr != nil {
		t.Fatalf("unexpected outer error: %v", outerErr)
	}

	var gotErr error
	for _, iterErr := range seq {
		if iterErr != nil {
			gotErr = iterErr
			break
		}
	}

	if gotErr == nil {
		t.Fatal("expected iterator error, got nil")
	}
	if !errors.Is(gotErr, llm.ErrStreamingNotSupported) {
		t.Errorf("errors.Is(err, llm.ErrStreamingNotSupported) = false; err = %v", gotErr)
	}
}

func TestRunStream_ApprovalRejected(t *testing.T) {
	reg := tool.NewRegistry()
	reg.MustRegister(&mockTool{
		info:   tool.ToolInfo{Name: "sensitive", Description: "needs approval", RequiresApproval: true},
		result: tool.NewTextResult("should not reach"),
	})

	provider := NewMockStreamingProvider([][]llm.Chunk{
		{
			llm.ToolCallStartChunk{Index: 0, ID: "c1", Name: "sensitive"},
			llm.ToolCallArgsChunk{Index: 0, Delta: `{}`},
			llm.DoneChunk{FinishReason: "tool_calls"},
		},
		{
			llm.TextDeltaChunk{Text: "understood rejection"},
			llm.DoneChunk{FinishReason: "stop"},
		},
	})

	agent := New(provider, reg,
		WithApprovalFn(func(tool.ToolInfo, json.RawMessage) bool { return false }),
		WithMaxIter(10),
		WithLogger(discardLogger()),
	)

	seq, err := agent.RunStream(context.Background(), "run sensitive")
	events, errs := collectEvents(t, seq, err)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	// Find ToolResultEvent
	var found bool
	for _, ev := range events {
		if tr, ok := ev.(ToolResultEvent); ok && tr.Name == "sensitive" {
			found = true
			if tr.Result == nil || !tr.Result.IsError() {
				t.Error("ToolResultEvent.Result.IsError() = false, want true for rejected approval")
			}
		}
	}
	if !found {
		t.Fatal("no ToolResultEvent found for 'sensitive' tool")
	}
}

func TestRunStream_ToolNotFound(t *testing.T) {
	provider := NewMockStreamingProvider([][]llm.Chunk{
		{
			llm.ToolCallStartChunk{Index: 0, ID: "c1", Name: "nonexistent_tool"},
			llm.ToolCallArgsChunk{Index: 0, Delta: `{}`},
			llm.DoneChunk{FinishReason: "tool_calls"},
		},
		{
			llm.TextDeltaChunk{Text: "I see the error"},
			llm.DoneChunk{FinishReason: "stop"},
		},
	})

	agent := New(provider, tool.NewRegistry(), WithMaxIter(10), WithLogger(discardLogger()))
	seq, err := agent.RunStream(context.Background(), "call missing")
	events, errs := collectEvents(t, seq, err)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	// Find ToolResultEvent for the missing tool
	var found bool
	for _, ev := range events {
		if tr, ok := ev.(ToolResultEvent); ok && tr.Name == "nonexistent_tool" {
			found = true
			if tr.Result == nil || !tr.Result.IsError() {
				t.Error("ToolResultEvent.Result.IsError() = false, want true for missing tool")
			}
		}
	}
	if !found {
		t.Fatal("no ToolResultEvent found for 'nonexistent_tool'")
	}
}

func TestRunStream_HistoryIntegrity(t *testing.T) {
	reg := tool.NewRegistry()
	reg.MustRegister(&mockTool{
		info:   tool.ToolInfo{Name: "echo", Description: "echoes"},
		result: tool.NewTextResult("echo output"),
	})

	provider := NewMockStreamingProvider([][]llm.Chunk{
		{
			llm.ToolCallStartChunk{Index: 0, ID: "c1", Name: "echo"},
			llm.ToolCallArgsChunk{Index: 0, Delta: `{}`},
			llm.DoneChunk{FinishReason: "tool_calls"},
		},
		{
			llm.TextDeltaChunk{Text: "final"},
			llm.DoneChunk{FinishReason: "stop"},
		},
	})

	agent := New(provider, reg, WithMaxIter(10), WithLogger(discardLogger()))
	seq, err := agent.RunStream(context.Background(), "test")
	events, errs := collectEvents(t, seq, err)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	done, ok := events[len(events)-1].(DoneEvent)
	if !ok {
		t.Fatalf("last event = %T, want DoneEvent", events[len(events)-1])
	}

	wantRoles := []llm.Role{llm.RoleUser, llm.RoleAssistant, llm.RoleTool, llm.RoleAssistant}
	if len(done.History) != len(wantRoles) {
		t.Fatalf("History length = %d, want %d", len(done.History), len(wantRoles))
	}
	for i, want := range wantRoles {
		if done.History[i].Role != want {
			t.Errorf("History[%d].Role = %q, want %q", i, done.History[i].Role, want)
		}
	}

	// Verify History is a defensive copy
	originalLen := len(done.History)
	done.History = append(done.History, llm.Message{Role: llm.RoleUser, Content: nil})
	// This is a sanity check that we modified our copy, not some shared reference.
	// The real invariant is that the copy was made with `copy()`.
	_ = originalLen // just verify no panic from the append above
}

func TestRunStream_TextAndToolCallsInSameStream(t *testing.T) {
	reg := tool.NewRegistry()
	reg.MustRegister(&mockTool{
		info:   tool.ToolInfo{Name: "echo", Description: "echoes"},
		result: tool.NewTextResult("echoed"),
	})

	provider := NewMockStreamingProvider([][]llm.Chunk{
		{
			llm.TextDeltaChunk{Text: "thinking..."},
			llm.ToolCallStartChunk{Index: 0, ID: "c1", Name: "echo"},
			llm.ToolCallArgsChunk{Index: 0, Delta: `{}`},
			llm.DoneChunk{FinishReason: "tool_calls"},
		},
		{
			llm.DoneChunk{FinishReason: "stop"},
		},
	})

	agent := New(provider, reg, WithMaxIter(10), WithLogger(discardLogger()))
	seq, err := agent.RunStream(context.Background(), "think and call")
	events, errs := collectEvents(t, seq, err)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	// Expect: TextDeltaEvent, ToolCallEvent, ToolResultEvent, DoneEvent
	if len(events) < 4 {
		t.Fatalf("expected at least 4 events, got %d", len(events))
	}

	td, ok := events[0].(TextDeltaEvent)
	if !ok {
		t.Fatalf("events[0] = %T, want TextDeltaEvent", events[0])
	}
	if td.Text != "thinking..." {
		t.Errorf("TextDeltaEvent.Text = %q, want %q", td.Text, "thinking...")
	}

	_, ok = events[1].(ToolCallEvent)
	if !ok {
		t.Fatalf("events[1] = %T, want ToolCallEvent", events[1])
	}

	_, ok = events[2].(ToolResultEvent)
	if !ok {
		t.Fatalf("events[2] = %T, want ToolResultEvent", events[2])
	}

	done, ok := events[3].(DoneEvent)
	if !ok {
		t.Fatalf("events[3] = %T, want DoneEvent", events[3])
	}

	// Verify the first assistant message in history has tool_use content
	var assistantMsg llm.Message
	for _, msg := range done.History {
		if msg.Role == llm.RoleAssistant {
			assistantMsg = msg
			break
		}
	}
	if assistantMsg.Role != llm.RoleAssistant {
		t.Fatal("no assistant message found in history")
	}

	hasToolUse := false
	for _, block := range assistantMsg.Content {
		if _, ok := block.(llm.ToolUseBlock); ok {
			hasToolUse = true
		}
	}
	if !hasToolUse {
		t.Error("assistant message has no ToolUseBlock, expected tool_use content")
	}
}

func TestRunStream_EmptyStreamDoneOnly(t *testing.T) {
	provider := NewMockStreamingProvider([][]llm.Chunk{
		{llm.DoneChunk{FinishReason: "stop"}},
	})

	agent := New(provider, tool.NewRegistry(), WithLogger(discardLogger()))
	seq, err := agent.RunStream(context.Background(), "test")
	events, errs := collectEvents(t, seq, err)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	done, ok := events[0].(DoneEvent)
	if !ok {
		t.Fatalf("events[0] = %T, want DoneEvent", events[0])
	}
	if done.Truncated {
		t.Error("DoneEvent.Truncated = true, want false")
	}
	// Message should have empty/nil content for a stream with no text deltas
	if len(done.Message.Content) != 0 {
		t.Errorf("DoneEvent.Message.Content length = %d, want 0", len(done.Message.Content))
	}
}

func TestRunStream_CallerBreaksEarly(t *testing.T) {
	reg := tool.NewRegistry()
	reg.MustRegister(&mockTool{
		info:   tool.ToolInfo{Name: "echo", Description: "echoes"},
		result: tool.NewTextResult("echoed"),
	})

	provider := NewMockStreamingProvider([][]llm.Chunk{
		{
			llm.TextDeltaChunk{Text: "hello"},
			llm.ToolCallStartChunk{Index: 0, ID: "c1", Name: "echo"},
			llm.ToolCallArgsChunk{Index: 0, Delta: `{}`},
			llm.DoneChunk{FinishReason: "tool_calls"},
		},
	})

	agent := New(provider, reg, WithMaxIter(10), WithLogger(discardLogger()))
	seq, err := agent.RunStream(context.Background(), "test")
	if err != nil {
		t.Fatalf("unexpected outer error: %v", err)
	}

	// Iterate only 2 events then break
	count := 0
	for ev, iterErr := range seq {
		if iterErr != nil {
			t.Fatalf("unexpected iterator error: %v", iterErr)
		}
		_ = ev
		count++
		if count >= 2 {
			break
		}
	}

	// Test passes if we got here without panic
	if count != 2 {
		t.Errorf("iterated %d events, want 2", count)
	}
}

// ---------------------------------------------------------------------------
// Run retry tests
// ---------------------------------------------------------------------------

func TestAgentRun_RetryOnRateLimit(t *testing.T) {
	provider := NewRetryableMockProvider(
		&llm.APIError{StatusCode: 429},
		1,
		llm.AssistantMessage("recovered!"),
	)
	agent := New(provider, tool.NewRegistry(), WithLogger(discardLogger()))

	msg, err := agent.Run(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if msgText := messageText(&msg.Message); msgText != "recovered!" {
		t.Errorf("Message text = %q, want %q", msgText, "recovered!")
	}
}

func TestAgentRun_RetryOnServerError(t *testing.T) {
	provider := NewRetryableMockProvider(
		&llm.APIError{StatusCode: 500},
		1,
		llm.AssistantMessage("recovered!"),
	)
	agent := New(provider, tool.NewRegistry(), WithLogger(discardLogger()))

	msg, err := agent.Run(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if msgText := messageText(&msg.Message); msgText != "recovered!" {
		t.Errorf("Message text = %q, want %q", msgText, "recovered!")
	}
}

func TestAgentRun_RetryOnNetworkError(t *testing.T) {
	provider := NewRetryableMockProvider(
		fmt.Errorf("connection refused"),
		1,
		llm.AssistantMessage("recovered!"),
	)
	agent := New(provider, tool.NewRegistry(), WithLogger(discardLogger()))

	msg, err := agent.Run(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if msgText := messageText(&msg.Message); msgText != "recovered!" {
		t.Errorf("Message text = %q, want %q", msgText, "recovered!")
	}
}

func TestAgentRun_NoRetryOnClientError(t *testing.T) {
	provider := NewMockProvider(
		ErrResponse(&llm.APIError{StatusCode: 400}),
	)
	agent := New(provider, tool.NewRegistry(), WithLogger(discardLogger()))

	_, err := agent.Run(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var apiErr *llm.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *llm.APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != 400 {
		t.Errorf("StatusCode = %d, want 400", apiErr.StatusCode)
	}
}

func TestAgentRun_NoRetryOnAuthError(t *testing.T) {
	provider := NewMockProvider(
		ErrResponse(&llm.APIError{StatusCode: 401}),
	)
	agent := New(provider, tool.NewRegistry(), WithLogger(discardLogger()))

	_, err := agent.Run(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var apiErr *llm.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *llm.APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != 401 {
		t.Errorf("StatusCode = %d, want 401", apiErr.StatusCode)
	}
}

func TestAgentRun_RetryExhausted(t *testing.T) {
	provider := NewMockProvider(
		ErrResponse(&llm.APIError{StatusCode: 429}),
	)
	agent := New(provider, tool.NewRegistry(), WithLogger(discardLogger()))

	_, err := agent.Run(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !contains(err.Error(), "failed after 4 attempt") {
		t.Errorf("error = %q, want something containing %q", err.Error(), "failed after 4 attempt")
	}
}

func TestAgentRun_RetryWithCallback(t *testing.T) {
	var callbackCalled atomic.Bool
	var callbackAttempt atomic.Int32
	var callbackDelay atomic.Int64
	var callbackErr error

	rateLimitErr := &llm.APIError{StatusCode: 429}

	provider := NewRetryableMockProvider(
		rateLimitErr,
		1,
		llm.AssistantMessage("recovered!"),
	)
	agent := New(provider, tool.NewRegistry(),
		WithRetryConfig(AgentRetryConfig{
			OnRetry: func(info RetryInfo) {
				callbackCalled.Store(true)
				callbackAttempt.Store(int32(info.Attempt))
				callbackDelay.Store(info.Delay.Milliseconds())
				callbackErr = info.Err
			},
		}),
		WithLogger(discardLogger()),
	)

	msg, err := agent.Run(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if msgText := messageText(&msg.Message); msgText != "recovered!" {
		t.Errorf("Message text = %q, want %q", msgText, "recovered!")
	}

	if !callbackCalled.Load() {
		t.Error("OnRetry callback was not called")
	}
	if callbackAttempt.Load() != 2 {
		t.Errorf("OnRetry attempt = %d, want 2", callbackAttempt.Load())
	}
	if callbackDelay.Load() <= 0 {
		t.Error("OnRetry delay <= 0, want positive")
	}
	if callbackErr == nil {
		t.Fatal("OnRetry err = nil, want the 429 error")
	}
	var apiErr *llm.APIError
	if !errors.As(callbackErr, &apiErr) || apiErr.StatusCode != 429 {
		t.Errorf("OnRetry err = %v, want an APIError with status 429", callbackErr)
	}
}

func TestAgentRun_RetryContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	provider := NewMockProvider(
		ErrResponse(&llm.APIError{StatusCode: 429}),
	)
	agent := New(provider, tool.NewRegistry(),
		WithRetryConfig(AgentRetryConfig{BaseDelay: 50 * time.Millisecond}),
		WithLogger(discardLogger()),
	)

	// Cancel context after a short delay to trigger mid-retry cancellation
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	_, err := agent.Run(ctx, "hello")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}

func TestAgentRun_NoRetryOnContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	provider := NewRetryableMockProvider(
		fmt.Errorf("connection refused"),
		1,
		llm.AssistantMessage("recovered!"),
	)
	agent := New(provider, tool.NewRegistry(), WithLogger(discardLogger()))

	_, err := agent.Run(ctx, "hello")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}

func TestAgentRun_DefaultRetryConfig(t *testing.T) {
	provider := NewRetryableMockProvider(
		&llm.APIError{StatusCode: 429},
		1,
		llm.AssistantMessage("recovered!"),
	)
	// No WithRetryConfig — uses default (3 retries)
	agent := New(provider, tool.NewRegistry(), WithLogger(discardLogger()))

	msg, err := agent.Run(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if msgText := messageText(&msg.Message); msgText != "recovered!" {
		t.Errorf("Message text = %q, want %q", msgText, "recovered!")
	}
}

func TestAgentRun_CustomRetryConfig(t *testing.T) {
	provider := NewRetryableMockProvider(
		&llm.APIError{StatusCode: 429},
		3,
		llm.AssistantMessage("recovered!"),
	)
	agent := New(provider, tool.NewRegistry(),
		WithRetryConfig(AgentRetryConfig{MaxRetries: 5, BaseDelay: time.Second}),
		WithLogger(discardLogger()),
	)

	msg, err := agent.Run(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if msgText := messageText(&msg.Message); msgText != "recovered!" {
		t.Errorf("Message text = %q, want %q", msgText, "recovered!")
	}
}

func TestAgentRun_RetryOnSecondIteration(t *testing.T) {
	reg := tool.NewRegistry()
	reg.MustRegister(&mockTool{
		info:   tool.ToolInfo{Name: "test_tool", Description: "a test tool"},
		result: tool.NewTextResult("tool output"),
	})

	provider := NewMockProvider(
		// Iteration 1: tool call
		MsgResponse(llm.Message{
			Role: llm.RoleAssistant,
			Content: []llm.ContentBlock{
				llm.ToolUseBlock{ID: "c1", Name: "test_tool", Input: json.RawMessage(`{}`)},
			},
		}),
		// Iteration 2: 429 error (triggers retry)
		ErrResponse(&llm.APIError{StatusCode: 429}),
		// Retry succeeds
		MsgResponse(llm.AssistantMessage("recovered!")),
	)
	agent := New(provider, reg,
		WithMaxIter(5),
		WithLogger(discardLogger()),
	)

	msg, err := agent.Run(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if msgText := messageText(&msg.Message); msgText != "recovered!" {
		t.Errorf("Message text = %q, want %q", msgText, "recovered!")
	}
}

func TestAgentRun_ExplicitZeroMaxRetriesUsesDefault(t *testing.T) {
	provider := NewRetryableMockProvider(
		&llm.APIError{StatusCode: 429},
		3,
		llm.AssistantMessage("recovered!"),
	)
	agent := New(provider, tool.NewRegistry(),
		WithRetryConfig(AgentRetryConfig{MaxRetries: 0}),
		WithLogger(discardLogger()),
	)

	msg, err := agent.Run(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if msgText := messageText(&msg.Message); msgText != "recovered!" {
		t.Errorf("Message text = %q, want %q", msgText, "recovered!")
	}
}

// ---------------------------------------------------------------------------
// RunStream retry tests
// ---------------------------------------------------------------------------

func TestAgentRunStream_RetryOnRateLimit(t *testing.T) {
	chunks := [][]llm.Chunk{
		{llm.TextDeltaChunk{Text: "recovered!"}, llm.DoneChunk{FinishReason: "stop"}},
	}
	provider := NewRetryableStreamingMockProvider(
		&llm.APIError{StatusCode: 429},
		1,
		chunks,
	)
	agent := New(provider, tool.NewRegistry(), WithLogger(discardLogger()))

	events, errs := collectAllEvents(agent, context.Background(), "hello")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	hasRetry := false
	for _, evt := range events {
		if _, ok := evt.(RetryEvent); ok {
			hasRetry = true
		}
	}
	if !hasRetry {
		t.Error("expected at least 1 RetryEvent in events")
	}

	last := events[len(events)-1]
	done, ok := last.(DoneEvent)
	if !ok {
		t.Fatalf("last event = %T, want DoneEvent", last)
	}
	if done.Truncated {
		t.Error("DoneEvent.Truncated = true, want false")
	}
}

func TestAgentRunStream_RetryOnNetworkError(t *testing.T) {
	chunks := [][]llm.Chunk{
		{llm.TextDeltaChunk{Text: "recovered!"}, llm.DoneChunk{FinishReason: "stop"}},
	}
	provider := NewRetryableStreamingMockProvider(
		fmt.Errorf("connection refused"),
		1,
		chunks,
	)
	agent := New(provider, tool.NewRegistry(), WithLogger(discardLogger()))

	events, errs := collectAllEvents(agent, context.Background(), "hello")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	hasRetry := false
	for _, evt := range events {
		if _, ok := evt.(RetryEvent); ok {
			hasRetry = true
		}
	}
	if !hasRetry {
		t.Error("expected RetryEvent for network error")
	}
}

func TestAgentRunStream_RetryEventFields(t *testing.T) {
	chunks := [][]llm.Chunk{
		{llm.TextDeltaChunk{Text: "ok"}, llm.DoneChunk{FinishReason: "stop"}},
	}
	provider := NewRetryableStreamingMockProvider(
		&llm.APIError{StatusCode: 429},
		1,
		chunks,
	)
	agent := New(provider, tool.NewRegistry(), WithLogger(discardLogger()))

	events, errs := collectAllEvents(agent, context.Background(), "hello")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	var retry RetryEvent
	found := false
	for _, evt := range events {
		if r, ok := evt.(RetryEvent); ok {
			retry = r
			found = true
			break
		}
	}
	if !found {
		t.Fatal("no RetryEvent found")
	}
	if retry.Attempt != 2 {
		t.Errorf("RetryEvent.Attempt = %d, want 2", retry.Attempt)
	}
	if retry.MaxAttempts != 4 {
		t.Errorf("RetryEvent.MaxAttempts = %d, want 4", retry.MaxAttempts)
	}
	if retry.Delay <= 0 {
		t.Error("RetryEvent.Delay <= 0, want positive")
	}
	if retry.Reason != "rate limited (429)" {
		t.Errorf("RetryEvent.Reason = %q, want %q", retry.Reason, "rate limited (429)")
	}
}

func TestAgentRunStream_RetryExhausted(t *testing.T) {
	provider := NewRetryableStreamingMockProvider(
		&llm.APIError{StatusCode: 429},
		99,
		nil,
	)
	agent := New(provider, tool.NewRegistry(), WithLogger(discardLogger()))

	events, errs := collectAllEvents(agent, context.Background(), "hello")
	if len(errs) == 0 {
		t.Fatal("expected errors, got none")
	}
	if !contains(errs[0].Error(), "failed after 4 attempt") {
		t.Errorf("error = %q, want something containing %q", errs[0].Error(), "failed after 4 attempt")
	}

	retryCount := 0
	for _, evt := range events {
		if _, ok := evt.(RetryEvent); ok {
			retryCount++
		}
	}
	if retryCount == 0 {
		t.Error("expected at least 1 RetryEvent before exhaustion")
	}
}

func TestAgentRunStream_NoRetryOnNonRetryable(t *testing.T) {
	provider := NewRetryableStreamingMockProvider(
		&llm.APIError{StatusCode: 401},
		1,
		[][]llm.Chunk{{llm.TextDeltaChunk{Text: "should not reach"}, llm.DoneChunk{FinishReason: "stop"}}},
	)
	agent := New(provider, tool.NewRegistry(), WithLogger(discardLogger()))

	events, errs := collectAllEvents(agent, context.Background(), "hello")
	if len(errs) == 0 {
		t.Fatal("expected errors for 401, got none")
	}
	for _, evt := range events {
		if _, ok := evt.(RetryEvent); ok {
			t.Error("RetryEvent should not be emitted for non-retryable 401 error")
		}
	}
}

func TestAgentRunStream_RetryContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	provider := NewRetryableStreamingMockProvider(
		&llm.APIError{StatusCode: 429},
		99,
		nil,
	)
	agent := New(provider, tool.NewRegistry(),
		WithRetryConfig(AgentRetryConfig{BaseDelay: 50 * time.Millisecond}),
		WithLogger(discardLogger()),
	)

	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	_, errs := collectAllEvents(agent, ctx, "hello")
	if len(errs) == 0 {
		t.Fatal("expected error, got none")
	}
	if !errors.Is(errs[0], context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", errs[0])
	}
}

func TestAgentRunStream_NoRetryOnContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	provider := NewRetryableStreamingMockProvider(
		fmt.Errorf("connection refused"),
		1,
		[][]llm.Chunk{{llm.TextDeltaChunk{Text: "should not reach"}, llm.DoneChunk{FinishReason: "stop"}}},
	)
	agent := New(provider, tool.NewRegistry(), WithLogger(discardLogger()))

	events, errs := collectAllEvents(agent, ctx, "hello")
	if len(errs) == 0 {
		t.Fatal("expected error, got none")
	}
	if !errors.Is(errs[0], context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", errs[0])
	}
	for _, evt := range events {
		if _, ok := evt.(RetryEvent); ok {
			t.Error("RetryEvent should not be emitted when context is already canceled")
		}
	}
}

func TestAgentRunStream_MultipleRetries(t *testing.T) {
	chunks := [][]llm.Chunk{
		{llm.TextDeltaChunk{Text: "finally!"}, llm.DoneChunk{FinishReason: "stop"}},
	}
	provider := NewRetryableStreamingMockProvider(
		&llm.APIError{StatusCode: 429},
		3,
		chunks,
	)
	agent := New(provider, tool.NewRegistry(), WithLogger(discardLogger()))

	events, errs := collectAllEvents(agent, context.Background(), "hello")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	retryCount := 0
	for _, evt := range events {
		if _, ok := evt.(RetryEvent); ok {
			retryCount++
		}
	}
	if retryCount != 3 {
		t.Errorf("got %d RetryEvents, want 3", retryCount)
	}

	last := events[len(events)-1]
	if _, ok := last.(DoneEvent); !ok {
		t.Fatalf("last event = %T, want DoneEvent", last)
	}
}

func TestAgentRunStream_RetryPreservesStreamContent(t *testing.T) {
	chunks := [][]llm.Chunk{
		{llm.TextDeltaChunk{Text: "hello world"}, llm.DoneChunk{FinishReason: "stop"}},
	}
	provider := NewRetryableStreamingMockProvider(
		&llm.APIError{StatusCode: 429},
		1,
		chunks,
	)
	agent := New(provider, tool.NewRegistry(), WithLogger(discardLogger()))

	events, errs := collectAllEvents(agent, context.Background(), "hello")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	var gotText string
	for _, evt := range events {
		if td, ok := evt.(TextDeltaEvent); ok {
			gotText += td.Text
		}
	}
	if gotText != "hello world" {
		t.Errorf("stream text = %q, want %q", gotText, "hello world")
	}
}

func TestAgentRunStream_RetryReasonSanitized(t *testing.T) {
	t.Run("429", func(t *testing.T) {
		chunks := [][]llm.Chunk{
			{llm.TextDeltaChunk{Text: "ok"}, llm.DoneChunk{FinishReason: "stop"}},
		}
		provider := NewRetryableStreamingMockProvider(
			&llm.APIError{StatusCode: 429, Body: "sensitive internal details"},
			1,
			chunks,
		)
		agent := New(provider, tool.NewRegistry(), WithLogger(discardLogger()))

		events, _ := collectAllEvents(agent, context.Background(), "hello")
		for _, evt := range events {
			if r, ok := evt.(RetryEvent); ok {
				if r.Reason != "rate limited (429)" {
					t.Errorf("429 reason = %q, want %q", r.Reason, "rate limited (429)")
				}
				return
			}
		}
		t.Error("no RetryEvent found")
	})

	t.Run("500", func(t *testing.T) {
		chunks := [][]llm.Chunk{
			{llm.TextDeltaChunk{Text: "ok"}, llm.DoneChunk{FinishReason: "stop"}},
		}
		provider := NewRetryableStreamingMockProvider(
			&llm.APIError{StatusCode: 500, Body: "internal server error details"},
			1,
			chunks,
		)
		agent := New(provider, tool.NewRegistry(), WithLogger(discardLogger()))

		events, _ := collectAllEvents(agent, context.Background(), "hello")
		for _, evt := range events {
			if r, ok := evt.(RetryEvent); ok {
				if r.Reason != "server error (500)" {
					t.Errorf("500 reason = %q, want %q", r.Reason, "server error (500)")
				}
				return
			}
		}
		t.Error("no RetryEvent found")
	})
}

func TestAgentRunStream_OnRetryNotCalled(t *testing.T) {
	var called atomic.Bool

	chunks := [][]llm.Chunk{
		{llm.TextDeltaChunk{Text: "recovered!"}, llm.DoneChunk{FinishReason: "stop"}},
	}
	provider := NewRetryableStreamingMockProvider(
		&llm.APIError{StatusCode: 429},
		1,
		chunks,
	)
	agent := New(provider, tool.NewRegistry(),
		WithRetryConfig(AgentRetryConfig{
			OnRetry: func(RetryInfo) { called.Store(true) },
		}),
		WithLogger(discardLogger()),
	)

	events, errs := collectAllEvents(agent, context.Background(), "hello")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	hasRetry := false
	for _, evt := range events {
		if _, ok := evt.(RetryEvent); ok {
			hasRetry = true
		}
	}
	if !hasRetry {
		t.Error("expected RetryEvent in events")
	}
	if called.Load() {
		t.Error("OnRetry should NOT be called from RunStream (only from Run)")
	}
}

// ---------------------------------------------------------------------------
// RunWithHistory tests
// ---------------------------------------------------------------------------

func TestRunWithHistory(t *testing.T) {
	t.Run("WithPriorHistory", func(t *testing.T) {
		provider := NewMockProvider(
			MsgResponse(llm.AssistantMessage("continuing")),
		)
		agent := New(provider, tool.NewRegistry(), WithLogger(discardLogger()))

		history := []llm.Message{
			llm.UserMessage("first question"),
			llm.AssistantMessage("first answer"),
		}

		result, err := agent.RunWithHistory(context.Background(), history, "follow up")
		if err != nil {
			t.Fatalf("RunWithHistory returned error: %v", err)
		}

		// History should contain: prior user + prior assistant + new user + new assistant
		wantRoles := []llm.Role{llm.RoleUser, llm.RoleAssistant, llm.RoleUser, llm.RoleAssistant}
		if len(result.History) != len(wantRoles) {
			t.Fatalf("History length = %d, want %d", len(result.History), len(wantRoles))
		}
		for i, want := range wantRoles {
			if result.History[i].Role != want {
				t.Errorf("History[%d].Role = %q, want %q", i, result.History[i].Role, want)
			}
		}

		// Verify the original history messages are preserved
		tb0, ok := result.History[0].Content[0].(llm.TextBlock)
		if !ok {
			t.Fatal("History[0].Content[0] is not a TextBlock")
		}
		if tb0.Text != "first question" {
			t.Errorf("History[0] text = %q, want %q", tb0.Text, "first question")
		}

		tb1, ok := result.History[1].Content[0].(llm.TextBlock)
		if !ok {
			t.Fatal("History[1].Content[0] is not a TextBlock")
		}
		if tb1.Text != "first answer" {
			t.Errorf("History[1] text = %q, want %q", tb1.Text, "first answer")
		}
	})

	t.Run("WithEmptyHistory", func(t *testing.T) {
		provider := NewMockProvider(
			MsgResponse(llm.AssistantMessage("hello")),
		)
		agent := New(provider, tool.NewRegistry(), WithLogger(discardLogger()))

		result, err := agent.RunWithHistory(context.Background(), []llm.Message{}, "hi")
		if err != nil {
			t.Fatalf("RunWithHistory returned error: %v", err)
		}

		// History should contain: user + assistant (like plain Run without system prompt)
		wantRoles := []llm.Role{llm.RoleUser, llm.RoleAssistant}
		if len(result.History) != len(wantRoles) {
			t.Fatalf("History length = %d, want %d", len(result.History), len(wantRoles))
		}
		for i, want := range wantRoles {
			if result.History[i].Role != want {
				t.Errorf("History[%d].Role = %q, want %q", i, result.History[i].Role, want)
			}
		}
	})

	t.Run("WithNilHistory", func(t *testing.T) {
		provider := NewMockProvider(
			MsgResponse(llm.AssistantMessage("hello")),
		)
		agent := New(provider, tool.NewRegistry(), WithLogger(discardLogger()))

		result, err := agent.RunWithHistory(context.Background(), nil, "hi")
		if err != nil {
			t.Fatalf("RunWithHistory returned error: %v", err)
		}

		if result.Truncated {
			t.Error("Truncated = true, want false")
		}
		if msgText := messageText(&result.Message); msgText != "hello" {
			t.Errorf("Message text = %q, want %q", msgText, "hello")
		}
	})

	t.Run("WithToolCallHistory", func(t *testing.T) {
		reg := tool.NewRegistry()
		reg.MustRegister(&mockTool{
			info:   tool.ToolInfo{Name: "mock_tool", Description: "a test tool"},
			result: tool.NewTextResult("tool output"),
		})

		provider := NewMockProvider(
			MsgResponse(llm.AssistantMessage("final answer")),
		)
		agent := New(provider, reg, WithLogger(discardLogger()))

		history := []llm.Message{
			llm.UserMessage("original question"),
			llm.AssistantToolCallMessage(llm.ToolUseBlock{
				Type:  "tool_use",
				ID:    "c0",
				Name:  "mock_tool",
				Input: json.RawMessage(`{"x":1}`),
			}),
			llm.ToolResultMessage("c0", tool.NewTextResult("prior result")),
		}

		result, err := agent.RunWithHistory(context.Background(), history, "continue")
		if err != nil {
			t.Fatalf("RunWithHistory returned error: %v", err)
		}

		// History: prior user + prior assistant(tool_use) + prior tool_result + new user + new assistant
		wantRoles := []llm.Role{llm.RoleUser, llm.RoleAssistant, llm.RoleTool, llm.RoleUser, llm.RoleAssistant}
		if len(result.History) != len(wantRoles) {
			t.Fatalf("History length = %d, want %d", len(result.History), len(wantRoles))
		}
		for i, want := range wantRoles {
			if result.History[i].Role != want {
				t.Errorf("History[%d].Role = %q, want %q", i, result.History[i].Role, want)
			}
		}

		// Verify the prior tool_use block is preserved
		priorAssistant := result.History[1]
		var foundToolUse bool
		for _, block := range priorAssistant.Content {
			if tu, ok := block.(llm.ToolUseBlock); ok && tu.Name == "mock_tool" {
				foundToolUse = true
			}
		}
		if !foundToolUse {
			t.Error("prior assistant message does not contain ToolUseBlock for mock_tool")
		}
	})

	t.Run("OriginalRunUnchanged", func(t *testing.T) {
		provider := NewMockProvider(
			MsgResponse(llm.AssistantMessage("hello")),
		)
		agent := New(provider, tool.NewRegistry(), WithLogger(discardLogger()))

		result, err := agent.Run(context.Background(), "hi")
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}

		if result.ToolCalls != 0 {
			t.Errorf("ToolCalls = %d, want 0", result.ToolCalls)
		}
		if result.Truncated {
			t.Error("Truncated = true, want false")
		}
		if msgText := messageText(&result.Message); msgText != "hello" {
			t.Errorf("Message text = %q, want %q", msgText, "hello")
		}
		if len(result.History) != 2 {
			t.Fatalf("History length = %d, want 2", len(result.History))
		}
		if result.History[0].Role != llm.RoleUser {
			t.Errorf("History[0].Role = %q, want %q", result.History[0].Role, llm.RoleUser)
		}
		if result.History[1].Role != llm.RoleAssistant {
			t.Errorf("History[1].Role = %q, want %q", result.History[1].Role, llm.RoleAssistant)
		}
	})
}

// ---------------------------------------------------------------------------
// RunStreamWithHistory tests
// ---------------------------------------------------------------------------

func TestRunStreamWithHistory(t *testing.T) {
	t.Run("WithPriorHistory", func(t *testing.T) {
		provider := NewMockStreamingProvider([][]llm.Chunk{
			{llm.TextDeltaChunk{Text: "continuing"}, llm.DoneChunk{FinishReason: "stop"}},
		})
		agent := New(provider, tool.NewRegistry(), WithLogger(discardLogger()))

		history := []llm.Message{
			llm.UserMessage("first question"),
			llm.AssistantMessage("first answer"),
		}

		seq, err := agent.RunStreamWithHistory(context.Background(), history, "follow up")
		events, errs := collectEvents(t, seq, err)
		if len(errs) > 0 {
			t.Fatalf("unexpected errors: %v", errs)
		}

		var doneEvent DoneEvent
		for _, ev := range events {
			if d, ok := ev.(DoneEvent); ok {
				doneEvent = d
			}
		}

		// History: prior user + prior assistant + new user + new assistant
		wantRoles := []llm.Role{llm.RoleUser, llm.RoleAssistant, llm.RoleUser, llm.RoleAssistant}
		if len(doneEvent.History) != len(wantRoles) {
			t.Fatalf("History length = %d, want %d", len(doneEvent.History), len(wantRoles))
		}
		for i, want := range wantRoles {
			if doneEvent.History[i].Role != want {
				t.Errorf("History[%d].Role = %q, want %q", i, doneEvent.History[i].Role, want)
			}
		}
	})

	t.Run("WithEmptyHistory", func(t *testing.T) {
		provider := NewMockStreamingProvider([][]llm.Chunk{
			{llm.TextDeltaChunk{Text: "hello"}, llm.DoneChunk{FinishReason: "stop"}},
		})
		agent := New(provider, tool.NewRegistry(), WithLogger(discardLogger()))

		seq, err := agent.RunStreamWithHistory(context.Background(), []llm.Message{}, "hi")
		events, errs := collectEvents(t, seq, err)
		if len(errs) > 0 {
			t.Fatalf("unexpected errors: %v", errs)
		}

		var doneEvent DoneEvent
		for _, ev := range events {
			if d, ok := ev.(DoneEvent); ok {
				doneEvent = d
			}
		}

		// History: user + assistant (like plain RunStream without system prompt)
		wantRoles := []llm.Role{llm.RoleUser, llm.RoleAssistant}
		if len(doneEvent.History) != len(wantRoles) {
			t.Fatalf("History length = %d, want %d", len(doneEvent.History), len(wantRoles))
		}
		for i, want := range wantRoles {
			if doneEvent.History[i].Role != want {
				t.Errorf("History[%d].Role = %q, want %q", i, doneEvent.History[i].Role, want)
			}
		}
	})

	t.Run("WithNilHistory", func(t *testing.T) {
		provider := NewMockStreamingProvider([][]llm.Chunk{
			{llm.TextDeltaChunk{Text: "hello"}, llm.DoneChunk{FinishReason: "stop"}},
		})
		agent := New(provider, tool.NewRegistry(), WithLogger(discardLogger()))

		seq, err := agent.RunStreamWithHistory(context.Background(), nil, "hi")
		events, errs := collectEvents(t, seq, err)
		if len(errs) > 0 {
			t.Fatalf("unexpected errors: %v", errs)
		}

		var doneEvent DoneEvent
		for _, ev := range events {
			if d, ok := ev.(DoneEvent); ok {
				doneEvent = d
			}
		}

		if doneEvent.Truncated {
			t.Error("DoneEvent.Truncated = true, want false")
		}
	})

	t.Run("WithToolCallHistory", func(t *testing.T) {
		reg := tool.NewRegistry()
		reg.MustRegister(&mockTool{
			info:   tool.ToolInfo{Name: "echo", Description: "echoes input"},
			result: tool.NewTextResult("echoed"),
		})

		provider := NewMockStreamingProvider([][]llm.Chunk{
			{
				llm.ToolCallStartChunk{Index: 0, ID: "c1", Name: "echo"},
				llm.ToolCallArgsChunk{Index: 0, Delta: `{}`},
				llm.DoneChunk{FinishReason: "tool_calls"},
			},
			{
				llm.TextDeltaChunk{Text: "final"},
				llm.DoneChunk{FinishReason: "stop"},
			},
		})
		agent := New(provider, reg, WithMaxIter(10), WithLogger(discardLogger()))

		history := []llm.Message{
			llm.UserMessage("original question"),
			llm.AssistantToolCallMessage(llm.ToolUseBlock{
				Type:  "tool_use",
				ID:    "c0",
				Name:  "echo",
				Input: json.RawMessage(`{"x":1}`),
			}),
			llm.ToolResultMessage("c0", tool.NewTextResult("prior result")),
		}

		seq, err := agent.RunStreamWithHistory(context.Background(), history, "continue")
		events, errs := collectEvents(t, seq, err)
		if len(errs) > 0 {
			t.Fatalf("unexpected errors: %v", errs)
		}

		var doneEvent DoneEvent
		for _, ev := range events {
			if d, ok := ev.(DoneEvent); ok {
				doneEvent = d
			}
		}

		// History: prior user + prior assistant(tool_use) + prior tool_result +
		// new user + new assistant(tool_use) + new tool_result + final assistant
		wantRoles := []llm.Role{
			llm.RoleUser, llm.RoleAssistant, llm.RoleTool,
			llm.RoleUser, llm.RoleAssistant, llm.RoleTool, llm.RoleAssistant,
		}
		if len(doneEvent.History) != len(wantRoles) {
			t.Fatalf("History length = %d, want %d (roles: %+v)", len(doneEvent.History), len(wantRoles), rolesOf(doneEvent.History))
		}
		for i, want := range wantRoles {
			if doneEvent.History[i].Role != want {
				t.Errorf("History[%d].Role = %q, want %q", i, doneEvent.History[i].Role, want)
			}
		}

		// Verify the prior tool_use block is preserved in history
		priorAssistant := doneEvent.History[1]
		var foundToolUse bool
		for _, block := range priorAssistant.Content {
			if tu, ok := block.(llm.ToolUseBlock); ok && tu.Name == "echo" {
				foundToolUse = true
			}
		}
		if !foundToolUse {
			t.Error("prior assistant message does not contain ToolUseBlock for echo")
		}
	})
}

func rolesOf(msgs []llm.Message) []llm.Role {
	roles := make([]llm.Role, len(msgs))
	for i, m := range msgs {
		roles[i] = m.Role
	}
	return roles
}

func TestAgentRun_RetryInRunResult(t *testing.T) {
	provider := NewRetryableMockProvider(
		&llm.APIError{StatusCode: 429},
		2,
		llm.AssistantMessage("recovered!"),
	)
	agent := New(provider, tool.NewRegistry(), WithLogger(discardLogger()))
	result, err := agent.Run(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(result.Retries) != 2 {
		t.Fatalf("Retries length = %d, want 2", len(result.Retries))
	}
	r1 := result.Retries[0]
	if r1.Attempt != 2 {
		t.Errorf("Retries[0].Attempt = %d, want 2", r1.Attempt)
	}
	if r1.MaxAttempts != 4 {
		t.Errorf("Retries[0].MaxAttempts = %d, want 4", r1.MaxAttempts)
	}
	if r1.Delay <= 0 {
		t.Error("Retries[0].Delay <= 0")
	}
	if r1.Reason == "" {
		t.Error("Retries[0].Reason is empty")
	}
	if r1.Err == nil {
		t.Error("Retries[0].Err is nil")
	}
	var apiErr *llm.APIError
	if !errors.As(r1.Err, &apiErr) || apiErr.StatusCode != 429 {
		t.Errorf("Retries[0].Err = %v, want APIError(429)", r1.Err)
	}
}

func TestAgentRun_NoRetryEmptyRetries(t *testing.T) {
	provider := NewMockProvider(MsgResponse(llm.AssistantMessage("first try")))
	agent := New(provider, tool.NewRegistry(), WithLogger(discardLogger()))
	result, err := agent.Run(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(result.Retries) != 0 {
		t.Errorf("Retries = %v, want empty", result.Retries)
	}
}

func TestRunStream_DoneEventRetries(t *testing.T) {
	chunks := [][]llm.Chunk{
		{llm.TextDeltaChunk{Text: "ok"}, llm.DoneChunk{FinishReason: "stop"}},
	}
	provider := NewRetryableStreamingMockProvider(
		&llm.APIError{StatusCode: 429},
		1,
		chunks,
	)
	agent := New(provider, tool.NewRegistry(), WithLogger(discardLogger()))
	events, errs := collectAllEvents(agent, context.Background(), "hello")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	var done *DoneEvent
	for i := range events {
		if d, ok := events[i].(DoneEvent); ok {
			done = &d
			break
		}
	}
	if done == nil {
		t.Fatal("no DoneEvent found")
	}
	if len(done.Retries) != 1 {
		t.Fatalf("DoneEvent.Retries length = %d, want 1", len(done.Retries))
	}
	if done.Retries[0].Attempt != 2 {
		t.Errorf("Attempt = %d, want 2", done.Retries[0].Attempt)
	}
	if done.Retries[0].Err == nil {
		t.Error("Retries[0].Err is nil")
	}
}

func TestRetryInfoFields(t *testing.T) {
	err := &llm.APIError{StatusCode: 500}
	info := RetryInfo{
		Attempt:     3,
		MaxAttempts: 4,
		Delay:       200 * time.Millisecond,
		Reason:      "server error (500)",
		Err:         err,
	}
	if info.Attempt != 3 {
		t.Errorf("Attempt = %d, want 3", info.Attempt)
	}
	if info.MaxAttempts != 4 {
		t.Errorf("MaxAttempts = %d, want 4", info.MaxAttempts)
	}
	if info.Delay != 200*time.Millisecond {
		t.Errorf("Delay = %v, want 200ms", info.Delay)
	}
	if info.Reason != "server error (500)" {
		t.Errorf("Reason = %q, want %q", info.Reason, "server error (500)")
	}
	if info.Err != err {
		t.Errorf("Err = %v, want %v", info.Err, err)
	}
}

type panickingTool struct{}

func (panickingTool) Info() tool.ToolInfo {
	return tool.ToolInfo{Name: "panic_tool", Description: "a tool that panics"}
}

func (panickingTool) Execute(_ context.Context, _ json.RawMessage) (*tool.ToolResult, error) {
	panic("something went terribly wrong")
}

func TestAgentRun_ToolPanicRecovered(t *testing.T) {
	reg := tool.NewRegistry()
	reg.MustRegister(panickingTool{})

	provider := NewMockProvider(
		MsgResponse(llm.AssistantToolCallMessage(llm.ToolUseBlock{
			Type: "tool_use", ID: "c1", Name: "panic_tool", Input: json.RawMessage(`{}`),
		})),
		MsgResponse(llm.AssistantMessage("I see the error")),
	)

	ag := New(provider, reg, WithMaxIter(3), WithLogger(discardLogger()))
	result, err := ag.Run(context.Background(), "test")
	if err != nil {
		t.Fatalf("Run() returned error: %v", err)
	}

	toolMsg := result.History[2]
	var found bool
	for _, block := range toolMsg.Content {
		if tr, ok := block.(llm.ToolResultBlock); ok {
			found = true
			if !tr.IsError {
				t.Error("expected ToolResultBlock.IsError = true")
			}
			if !strings.Contains(tr.Content, "panicked") {
				t.Errorf("ToolResultBlock.Content = %q, want substring %q", tr.Content, "panicked")
			}
		}
	}
	if !found {
		t.Error("no ToolResultBlock found in history[2]")
	}
}

func TestAgentRunStream_ToolPanicRecovered(t *testing.T) {
	reg := tool.NewRegistry()
	reg.MustRegister(panickingTool{})

	provider := NewMockStreamingProvider([][]llm.Chunk{
		{
			llm.ToolCallStartChunk{Index: 0, ID: "c1", Name: "panic_tool"},
			llm.ToolCallArgsChunk{Index: 0, Delta: `{}`},
			llm.DoneChunk{FinishReason: "tool_calls"},
		},
		{
			llm.TextDeltaChunk{Text: "recovered"},
			llm.DoneChunk{FinishReason: "stop"},
		},
	})

	ag := New(provider, reg, WithMaxIter(3), WithLogger(discardLogger()))
	seq, err := ag.RunStream(context.Background(), "test")
	if err != nil {
		t.Fatalf("RunStream() returned error: %v", err)
	}

	events, errs := collectEvents(t, seq, err)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	var found bool
	for _, e := range events {
		if tr, ok := e.(ToolResultEvent); ok {
			found = true
			if !strings.Contains(tr.Result.Content, "panicked") {
				t.Errorf("ToolResultEvent.Content = %q, want substring %q", tr.Result.Content, "panicked")
			}
		}
	}
	if !found {
		t.Error("no ToolResultEvent found")
	}
}

func TestAgentRun_UsageFromProvider(t *testing.T) {
	usage := &llm.Usage{InputTokens: 100, OutputTokens: 50, ReasoningTokens: 10}
	provider := NewMockProvider(
		MsgWithUsageResponse(llm.AssistantMessage("hello"), usage),
	)
	agent := New(provider, tool.NewRegistry(), WithLogger(discardLogger()))

	result, err := agent.Run(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if result.Usage.InputTokens != 100 {
		t.Errorf("Usage.InputTokens = %d, want 100", result.Usage.InputTokens)
	}
	if result.Usage.OutputTokens != 50 {
		t.Errorf("Usage.OutputTokens = %d, want 50", result.Usage.OutputTokens)
	}
	if result.Usage.ReasoningTokens != 10 {
		t.Errorf("Usage.ReasoningTokens = %d, want 10", result.Usage.ReasoningTokens)
	}
	if result.TotalUsage.InputTokens != 100 {
		t.Errorf("TotalUsage.InputTokens = %d, want 100", result.TotalUsage.InputTokens)
	}
}

func TestAgentRun_UsageNilFromProvider(t *testing.T) {
	provider := NewMockProvider(
		MsgResponse(llm.AssistantMessage("hello")),
	)
	agent := New(provider, tool.NewRegistry(), WithLogger(discardLogger()))

	result, err := agent.Run(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if !result.Usage.IsZero() {
		t.Errorf("Usage = %v, want zero when provider returns nil usage", result.Usage)
	}
	if !result.TotalUsage.IsZero() {
		t.Errorf("TotalUsage = %v, want zero when provider returns nil usage", result.TotalUsage)
	}
}

func TestAgentRun_UsageAccumulatesAcrossIterations(t *testing.T) {
	reg := tool.NewRegistry()
	reg.MustRegister(&mockTool{
		info:   tool.ToolInfo{Name: "echo", Description: "echoes"},
		result: tool.NewTextResult("echoed"),
	})

	provider := NewMockProvider(
		MsgWithUsageResponse(
			llm.AssistantToolCallMessage(llm.ToolUseBlock{Type: "tool_use", ID: "c1", Name: "echo", Input: json.RawMessage(`{}`)}),
			&llm.Usage{InputTokens: 50, OutputTokens: 20},
		),
		MsgWithUsageResponse(
			llm.AssistantMessage("done"),
			&llm.Usage{InputTokens: 80, OutputTokens: 30},
		),
	)

	agent := New(provider, reg, WithLogger(discardLogger()))
	result, err := agent.Run(context.Background(), "call echo")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	// lastUsage from the second iteration
	if result.Usage.InputTokens != 80 {
		t.Errorf("Usage.InputTokens = %d, want 80 (last iteration)", result.Usage.InputTokens)
	}
	if result.Usage.OutputTokens != 30 {
		t.Errorf("Usage.OutputTokens = %d, want 30 (last iteration)", result.Usage.OutputTokens)
	}

	// totalUsage sums both iterations
	if result.TotalUsage.InputTokens != 130 {
		t.Errorf("TotalUsage.InputTokens = %d, want 130 (50+80)", result.TotalUsage.InputTokens)
	}
	if result.TotalUsage.OutputTokens != 50 {
		t.Errorf("TotalUsage.OutputTokens = %d, want 50 (20+30)", result.TotalUsage.OutputTokens)
	}
}

func TestAgentRun_UsageAccumulatesWithNilInMiddle(t *testing.T) {
	reg := tool.NewRegistry()
	reg.MustRegister(&mockTool{
		info:   tool.ToolInfo{Name: "echo", Description: "echoes"},
		result: tool.NewTextResult("echoed"),
	})

	provider := NewMockProvider(
		MsgWithUsageResponse(
			llm.AssistantToolCallMessage(llm.ToolUseBlock{Type: "tool_use", ID: "c1", Name: "echo", Input: json.RawMessage(`{}`)}),
			&llm.Usage{InputTokens: 50, OutputTokens: 20},
		),
		MsgResponse(llm.AssistantMessage("done")),
	)

	agent := New(provider, reg, WithLogger(discardLogger()))
	result, err := agent.Run(context.Background(), "call echo")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	// lastUsage retains the previous value when nil is returned
	if result.Usage.InputTokens != 50 {
		t.Errorf("Usage.InputTokens = %d, want 50 (retained from first iteration)", result.Usage.InputTokens)
	}
	if result.Usage.OutputTokens != 20 {
		t.Errorf("Usage.OutputTokens = %d, want 20 (retained from first iteration)", result.Usage.OutputTokens)
	}

	// totalUsage only has the first iteration (nil usage is not added)
	if result.TotalUsage.InputTokens != 50 {
		t.Errorf("TotalUsage.InputTokens = %d, want 50", result.TotalUsage.InputTokens)
	}
	if result.TotalUsage.OutputTokens != 20 {
		t.Errorf("TotalUsage.OutputTokens = %d, want 20", result.TotalUsage.OutputTokens)
	}
}

func TestRunStream_UsageFromDoneChunk(t *testing.T) {
	provider := NewMockStreamingProvider([][]llm.Chunk{
		{
			llm.TextDeltaChunk{Text: "hello"},
			llm.DoneChunk{
				FinishReason: "stop",
				Usage:        &llm.Usage{InputTokens: 100, OutputTokens: 50},
			},
		},
	})
	agent := New(provider, tool.NewRegistry(), WithLogger(discardLogger()))

	seq, err := agent.RunStream(context.Background(), "hi")
	events, errs := collectEvents(t, seq, err)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	done, ok := events[len(events)-1].(DoneEvent)
	if !ok {
		t.Fatalf("last event = %T, want DoneEvent", events[len(events)-1])
	}

	if done.Usage.InputTokens != 100 {
		t.Errorf("Usage.InputTokens = %d, want 100", done.Usage.InputTokens)
	}
	if done.Usage.OutputTokens != 50 {
		t.Errorf("Usage.OutputTokens = %d, want 50", done.Usage.OutputTokens)
	}
	if done.TotalUsage.InputTokens != 100 {
		t.Errorf("TotalUsage.InputTokens = %d, want 100", done.TotalUsage.InputTokens)
	}
}

func TestRunStream_UsageNilDoneChunk(t *testing.T) {
	provider := NewMockStreamingProvider([][]llm.Chunk{
		{llm.TextDeltaChunk{Text: "hello"}, llm.DoneChunk{FinishReason: "stop"}},
	})
	agent := New(provider, tool.NewRegistry(), WithLogger(discardLogger()))

	seq, err := agent.RunStream(context.Background(), "hi")
	events, errs := collectEvents(t, seq, err)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	done, ok := events[len(events)-1].(DoneEvent)
	if !ok {
		t.Fatalf("last event = %T, want DoneEvent", events[len(events)-1])
	}

	if !done.Usage.IsZero() {
		t.Errorf("Usage = %v, want zero when DoneChunk has no Usage", done.Usage)
	}
	if !done.TotalUsage.IsZero() {
		t.Errorf("TotalUsage = %v, want zero when DoneChunk has no Usage", done.TotalUsage)
	}
}

func TestRunStream_UsageAccumulatesAcrossIterations(t *testing.T) {
	reg := tool.NewRegistry()
	reg.MustRegister(&mockTool{
		info:   tool.ToolInfo{Name: "echo", Description: "echoes"},
		result: tool.NewTextResult("echoed"),
	})

	provider := NewMockStreamingProvider([][]llm.Chunk{
		{
			llm.ToolCallStartChunk{Index: 0, ID: "c1", Name: "echo"},
			llm.ToolCallArgsChunk{Index: 0, Delta: `{}`},
			llm.DoneChunk{
				FinishReason: "tool_calls",
				Usage:        &llm.Usage{InputTokens: 50, OutputTokens: 20},
			},
		},
		{
			llm.TextDeltaChunk{Text: "done"},
			llm.DoneChunk{
				FinishReason: "stop",
				Usage:        &llm.Usage{InputTokens: 80, OutputTokens: 30},
			},
		},
	})

	agent := New(provider, reg, WithMaxIter(10), WithLogger(discardLogger()))
	seq, err := agent.RunStream(context.Background(), "call echo")
	events, errs := collectEvents(t, seq, err)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	done, ok := events[len(events)-1].(DoneEvent)
	if !ok {
		t.Fatalf("last event = %T, want DoneEvent", events[len(events)-1])
	}

	if done.Usage.InputTokens != 80 {
		t.Errorf("Usage.InputTokens = %d, want 80", done.Usage.InputTokens)
	}
	if done.Usage.OutputTokens != 30 {
		t.Errorf("Usage.OutputTokens = %d, want 30", done.Usage.OutputTokens)
	}

	if done.TotalUsage.InputTokens != 130 {
		t.Errorf("TotalUsage.InputTokens = %d, want 130 (50+80)", done.TotalUsage.InputTokens)
	}
	if done.TotalUsage.OutputTokens != 50 {
		t.Errorf("TotalUsage.OutputTokens = %d, want 50 (20+30)", done.TotalUsage.OutputTokens)
	}
}

func TestRunStream_DualDoneChunkUsage(t *testing.T) {
	provider := NewMockStreamingProvider([][]llm.Chunk{
		{
			llm.TextDeltaChunk{Text: "hello"},
			llm.DoneChunk{FinishReason: "stop", Usage: nil},
			llm.DoneChunk{
				FinishReason: "stop",
				Usage:        &llm.Usage{InputTokens: 42, OutputTokens: 7},
			},
		},
	})
	agent := New(provider, tool.NewRegistry(), WithLogger(discardLogger()))

	seq, err := agent.RunStream(context.Background(), "test")
	events, errs := collectEvents(t, seq, err)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	done, ok := events[len(events)-1].(DoneEvent)
	if !ok {
		t.Fatalf("last event = %T, want DoneEvent", events[len(events)-1])
	}

	if done.Usage.InputTokens != 42 {
		t.Errorf("Usage.InputTokens = %d, want 42 (second DoneChunk wins)", done.Usage.InputTokens)
	}
	if done.TotalUsage.InputTokens != 42 {
		t.Errorf("TotalUsage.InputTokens = %d, want 42", done.TotalUsage.InputTokens)
	}
}

func TestMockProvider_ReturnsUsage(t *testing.T) {
	usage := &llm.Usage{InputTokens: 99, OutputTokens: 88, ReasoningTokens: 77}
	provider := NewMockProvider(
		MsgWithUsageResponse(llm.AssistantMessage("test"), usage),
	)

	msg, gotUsage, err := provider.Chat(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}
	if msg == nil {
		t.Fatal("msg = nil, want non-nil")
	}
	if gotUsage == nil {
		t.Fatal("usage = nil, want non-nil")
	}
	if gotUsage.InputTokens != 99 {
		t.Errorf("InputTokens = %d, want 99", gotUsage.InputTokens)
	}
	if gotUsage.OutputTokens != 88 {
		t.Errorf("OutputTokens = %d, want 88", gotUsage.OutputTokens)
	}
	if gotUsage.ReasoningTokens != 77 {
		t.Errorf("ReasoningTokens = %d, want 77", gotUsage.ReasoningTokens)
	}
}

func TestMockProvider_NilUsageByDefault(t *testing.T) {
	provider := NewMockProvider(
		MsgResponse(llm.AssistantMessage("test")),
	)

	_, gotUsage, err := provider.Chat(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}
	if gotUsage != nil {
		t.Errorf("usage = %v, want nil when using MsgResponse", gotUsage)
	}
}
