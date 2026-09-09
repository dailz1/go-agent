package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/dailz1/go-agent/pkg/llm"
	"github.com/dailz1/go-agent/pkg/tool"
)

func TestAgentToolResultLimit(t *testing.T) {
	t.Run("default limit is enabled", func(t *testing.T) {
		content := strings.Repeat("x", 10_000)
		agent, _ := newToolResultLimitAgent(tool.NewTextResult(content))

		result, err := agent.Run(context.Background(), "use the tool")
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}

		got := singleToolResultContent(t, result.History)
		if runeCount := utf8.RuneCountInString(got); runeCount != 2_457 {
			t.Errorf("tool result rune count = %d, want 2457", runeCount)
		}
	})

	t.Run("configured context window sets limit", func(t *testing.T) {
		content := strings.Repeat("x", 10_000)
		agent, _ := newToolResultLimitAgent(
			tool.NewTextResult(content),
			WithContextWindowTokens(1_000),
		)

		result, err := agent.Run(context.Background(), "use the tool")
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}

		got := singleToolResultContent(t, result.History)
		if runeCount := utf8.RuneCountInString(got); runeCount != 300 {
			t.Errorf("tool result rune count = %d, want 300", runeCount)
		}
	})

	t.Run("large context windows do not overflow", func(t *testing.T) {
		maxAgent, _ := newToolResultLimitAgent(
			tool.NewTextResult("unused"),
			WithContextWindowTokens(math.MaxInt),
		)
		if got := maxAgent.toolResultRunes(); got <= 0 || got <= math.MaxInt/4 {
			t.Errorf("toolResultRunes() = %d, want positive and greater than %d", got, math.MaxInt/4)
		}

		largeAgent, _ := newToolResultLimitAgent(
			tool.NewTextResult("unused"),
			WithContextWindowTokens(1_000_000_000_000_000_000),
		)
		if got := largeAgent.toolResultRunes(); got != 300_000_000_000_000_000 {
			t.Errorf("toolResultRunes() = %d, want 300000000000000000", got)
		}
	})

	t.Run("event request and history have identical content", func(t *testing.T) {
		content := strings.Repeat("x", 10_000)
		agent, provider := newToolResultLimitAgent(
			tool.NewTextResult(content),
			WithContextWindowTokens(1_000),
		)

		seq, err := agent.RunStream(context.Background(), "use the tool")
		if err != nil {
			t.Fatalf("RunStream returned error: %v", err)
		}
		var eventContent string
		var done DoneEvent
		for event, streamErr := range seq {
			if streamErr != nil {
				t.Fatalf("RunStream emitted error: %v", streamErr)
			}
			switch e := event.(type) {
			case ToolResultEvent:
				eventContent = e.Result.Content
			case DoneEvent:
				done = e
			}
		}

		requestContent := singleToolResultContent(t, provider.LastMessages)
		historyContent := singleToolResultContent(t, done.History)
		if eventContent != requestContent || requestContent != historyContent {
			t.Errorf(
				"tool result content differs: event=%q request=%q history=%q",
				eventContent,
				requestContent,
				historyContent,
			)
		}
	})

	t.Run("head and tail survive in the next request", func(t *testing.T) {
		content := "HEAD_SENTINEL" + strings.Repeat("x", 10_000) + "TAIL_SENTINEL"
		agent, provider := newToolResultLimitAgent(
			tool.NewTextResult(content),
			WithContextWindowTokens(1_000),
		)

		if _, err := agent.Run(context.Background(), "use the tool"); err != nil {
			t.Fatalf("Run returned error: %v", err)
		}

		got := singleToolResultContent(t, provider.LastMessages)
		if !strings.Contains(got, "HEAD_SENTINEL") {
			t.Errorf("round-two tool result does not contain head sentinel: %q", got)
		}
		if !strings.Contains(got, "TAIL_SENTINEL") {
			t.Errorf("round-two tool result does not contain tail sentinel: %q", got)
		}
	})

	t.Run("consumer mutation cannot change history", func(t *testing.T) {
		content := strings.Repeat("x", 10_000)
		agent, provider := newToolResultLimitAgent(
			tool.NewTextResult(content),
			WithContextWindowTokens(1_000),
		)

		seq, err := agent.RunStream(context.Background(), "use the tool")
		if err != nil {
			t.Fatalf("RunStream returned error: %v", err)
		}
		var contentBeforeMutation string
		var done DoneEvent
		for event, streamErr := range seq {
			if streamErr != nil {
				t.Fatalf("RunStream emitted error: %v", streamErr)
			}
			switch e := event.(type) {
			case ToolResultEvent:
				contentBeforeMutation = e.Result.Content
				e.Result.Content = "consumer garbage"
			case DoneEvent:
				done = e
			}
		}

		if contentBeforeMutation == "" {
			t.Fatal("RunStream emitted no ToolResultEvent")
		}
		requestContent := singleToolResultContent(t, provider.LastMessages)
		historyContent := singleToolResultContent(t, done.History)
		if requestContent != contentBeforeMutation {
			t.Errorf("round-two request content = %q, want pre-mutation content %q", requestContent, contentBeforeMutation)
		}
		if historyContent != contentBeforeMutation {
			t.Errorf("DoneEvent history content = %q, want pre-mutation content %q", historyContent, contentBeforeMutation)
		}
	})

	t.Run("multiple tool results are capped independently", func(t *testing.T) {
		registry := tool.NewRegistry()
		registry.MustRegister(&mockTool{
			info:   tool.ToolInfo{Name: "first"},
			result: tool.NewTextResult(strings.Repeat("a", 10_000)),
		})
		registry.MustRegister(&mockTool{
			info:   tool.ToolInfo{Name: "second"},
			result: tool.NewTextResult(strings.Repeat("b", 20_000)),
		})
		provider := NewMockProvider(
			MsgResponse(llm.AssistantToolCallMessage(
				newToolCall("call-first", "first"),
				newToolCall("call-second", "second"),
			)),
			MsgResponse(llm.AssistantMessage("done")),
		)
		agent := New(
			provider,
			registry,
			WithLogger(discardLogger()),
			WithContextWindowTokens(1_000),
		)

		result, err := agent.Run(context.Background(), "use both tools")
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}

		contents := toolResultContents(result.History)
		if len(contents) != 2 {
			t.Fatalf("tool result count = %d, want 2", len(contents))
		}
		for i, got := range contents {
			if runeCount := utf8.RuneCountInString(got); runeCount != 300 {
				t.Errorf("tool result %d rune count = %d, want 300", i, runeCount)
			}
		}
	})

	t.Run("error status survives truncation", func(t *testing.T) {
		agent, _ := newToolResultLimitAgent(
			tool.NewErrorResult("%s", strings.Repeat("x", 10_000)),
			WithContextWindowTokens(1_000),
		)

		seq, err := agent.RunStream(context.Background(), "use the tool")
		if err != nil {
			t.Fatalf("RunStream returned error: %v", err)
		}
		var got *tool.ToolResult
		for event, streamErr := range seq {
			if streamErr != nil {
				t.Fatalf("RunStream emitted error: %v", streamErr)
			}
			if resultEvent, ok := event.(ToolResultEvent); ok {
				got = resultEvent.Result
			}
		}

		if got == nil {
			t.Fatal("RunStream emitted no ToolResultEvent")
		}
		if runeCount := utf8.RuneCountInString(got.Content); runeCount != 300 {
			t.Errorf("error result rune count = %d, want 300", runeCount)
		}
		if !got.IsError() {
			t.Error("truncated tool result IsError() = false, want true")
		}
	})

	t.Run("truncation emits structured warning", func(t *testing.T) {
		capture := &truncationLogCapture{}
		logger := slog.New(capture)
		content := strings.Repeat("x", 10_000)
		agent, _ := newToolResultLimitAgent(
			tool.NewTextResult(content),
			WithContextWindowTokens(1_000),
			WithLogger(logger),
		)

		if _, err := agent.Run(context.Background(), "use the tool"); err != nil {
			t.Fatalf("Run returned error: %v", err)
		}

		records := capture.recordsWithMessage("tool result truncated")
		if len(records) != 1 {
			t.Fatalf("truncation warning count = %d, want 1", len(records))
		}
		record := records[0]
		_, expectedOmitted := truncateToolResult(tool.NewTextResult(content), 300)
		assertLogString(t, record, "tool_name", "mock_tool")
		assertLogString(t, record, "tool_call_id", "call-1")
		assertLogInt(t, record, "omitted_runes", int64(expectedOmitted))
		assertLogInt(t, record, "total_runes", 10_000)
		assertLogInt(t, record, "cap_runes", 300)
	})

	t.Run("run and runstream are equivalent", func(t *testing.T) {
		firstUsage := llm.Usage{InputTokens: 10, OutputTokens: 2}
		finalUsage := llm.Usage{InputTokens: 20, OutputTokens: 4}
		newProvider := func() *MockProvider {
			return NewMockProvider(
				MsgWithUsageResponse(
					llm.AssistantToolCallMessage(newToolCall("call-1", "mock_tool")),
					&firstUsage,
				),
				MsgWithUsageResponse(llm.AssistantMessage("done"), &finalUsage),
			)
		}
		newAgent := func(provider *MockProvider) *Agent {
			registry := tool.NewRegistry()
			registry.MustRegister(&mockTool{
				info:   tool.ToolInfo{Name: "mock_tool"},
				result: tool.NewTextResult(strings.Repeat("x", 10_000)),
			})
			return New(
				provider,
				registry,
				WithLogger(discardLogger()),
				WithContextWindowTokens(1_000),
			)
		}

		runResult, err := newAgent(newProvider()).Run(context.Background(), "use the tool")
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}

		streamAgent := newAgent(newProvider())
		seq, err := streamAgent.RunStream(context.Background(), "use the tool")
		if err != nil {
			t.Fatalf("RunStream returned error: %v", err)
		}
		var streamResult DoneEvent
		for event, streamErr := range seq {
			if streamErr != nil {
				t.Fatalf("RunStream emitted error: %v", streamErr)
			}
			if done, ok := event.(DoneEvent); ok {
				streamResult = done
			}
		}

		if got, expected := messageText(&streamResult.Message), messageText(&runResult.Message); got != expected {
			t.Errorf("RunStream message = %q, want Run message %q", got, expected)
		}
		if streamResult.ToolCalls != runResult.ToolCalls {
			t.Errorf("RunStream ToolCalls = %d, want %d", streamResult.ToolCalls, runResult.ToolCalls)
		}
		if streamResult.Usage != runResult.Usage {
			t.Errorf("RunStream Usage = %+v, want %+v", streamResult.Usage, runResult.Usage)
		}
		if !reflect.DeepEqual(streamResult.History, runResult.History) {
			t.Errorf("RunStream History = %#v, want %#v", streamResult.History, runResult.History)
		}
	})
}

func newToolResultLimitAgent(result *tool.ToolResult, opts ...Option) (*Agent, *MockProvider) {
	registry := tool.NewRegistry()
	registry.MustRegister(&mockTool{
		info:   tool.ToolInfo{Name: "mock_tool"},
		result: result,
	})
	provider := NewMockProvider(
		MsgResponse(llm.AssistantToolCallMessage(newToolCall("call-1", "mock_tool"))),
		MsgResponse(llm.AssistantMessage("done")),
	)
	options := append([]Option{WithLogger(discardLogger())}, opts...)
	return New(provider, registry, options...), provider
}

func newToolCall(id string, name string) llm.ToolUseBlock {
	return llm.ToolUseBlock{
		Type:  "tool_use",
		ID:    id,
		Name:  name,
		Input: json.RawMessage(`{}`),
	}
}

func singleToolResultContent(t *testing.T, history []llm.Message) string {
	t.Helper()
	contents := toolResultContents(history)
	if len(contents) != 1 {
		t.Fatalf("tool result count = %d, want 1", len(contents))
	}
	return contents[0]
}

func toolResultContents(history []llm.Message) []string {
	contents := []string{}
	for _, message := range history {
		for _, block := range message.Content {
			if resultBlock, ok := block.(llm.ToolResultBlock); ok {
				contents = append(contents, resultBlock.Content)
			}
		}
	}
	return contents
}

type truncationLogRecord struct {
	message string
	attrs   map[string]slog.Value
}

type truncationLogCapture struct {
	records []truncationLogRecord
}

func (h *truncationLogCapture) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelWarn
}

func (h *truncationLogCapture) Handle(_ context.Context, record slog.Record) error {
	attrs := map[string]slog.Value{}
	record.Attrs(func(attr slog.Attr) bool {
		attrs[attr.Key] = attr.Value
		return true
	})
	h.records = append(h.records, truncationLogRecord{message: record.Message, attrs: attrs})
	return nil
}

func (h *truncationLogCapture) WithAttrs(_ []slog.Attr) slog.Handler {
	return h
}

func (h *truncationLogCapture) WithGroup(_ string) slog.Handler {
	return h
}

func (h *truncationLogCapture) recordsWithMessage(message string) []truncationLogRecord {
	records := []truncationLogRecord{}
	for _, record := range h.records {
		if record.message == message {
			records = append(records, record)
		}
	}
	return records
}

func assertLogString(t *testing.T, record truncationLogRecord, key string, expected string) {
	t.Helper()
	value, ok := record.attrs[key]
	if !ok {
		t.Errorf("log record missing %q", key)
		return
	}
	if got := value.String(); got != expected {
		t.Errorf("log attribute %q = %q, want %q", key, got, expected)
	}
}

func assertLogInt(t *testing.T, record truncationLogRecord, key string, expected int64) {
	t.Helper()
	value, ok := record.attrs[key]
	if !ok {
		t.Errorf("log record missing %q", key)
		return
	}
	if got := value.Int64(); got != expected {
		t.Errorf("log attribute %q = %d, want %d", key, got, expected)
	}
}
