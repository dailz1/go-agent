//go:build e2e
// +build e2e

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dailz1/go-agent/llm/openai"
	"github.com/dailz1/go-agent/tool"
)

// ---------------------------------------------------------------------------
// Tools copied from cmd/demo (main package — cannot import)
// ---------------------------------------------------------------------------

type calculateTool struct{}

func (calculateTool) Info() tool.ToolInfo {
	schema := tool.NewParameterSchema()
	schema.Properties["expression"] = tool.Param("string", "Mathematical expression to evaluate, e.g. 2+3*4")
	schema.Required = []string{"expression"}
	return tool.ToolInfo{
		Name:        "calculate",
		Description: "Evaluates a simple arithmetic expression (supports +, -, *, /, parentheses)",
		Parameters:  schema,
	}
}

func (calculateTool) Execute(_ context.Context, args json.RawMessage) (*tool.ToolResult, error) {
	var params struct {
		Expression string `json:"expression"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return tool.NewErrorResult("invalid arguments: %s", err), nil
	}
	if params.Expression == "" {
		return tool.NewErrorResult("expression is required"), nil
	}
	result, err := evalSimple(params.Expression)
	if err != nil {
		return tool.NewErrorResult("eval error: %s", err), nil
	}
	return tool.NewTextResult(fmt.Sprintf("%g", result)), nil
}

func evalSimple(expr string) (float64, error) {
	p := &parser{input: strings.ReplaceAll(expr, " ", "")}
	result, err := p.parseExpr()
	if err != nil {
		return 0, err
	}
	if p.pos < len(p.input) {
		return 0, fmt.Errorf("unexpected character %q at position %d", p.input[p.pos], p.pos)
	}
	return result, nil
}

type parser struct {
	input string
	pos   int
}

func (p *parser) peek() byte {
	if p.pos >= len(p.input) {
		return 0
	}
	return p.input[p.pos]
}

func (p *parser) next() byte {
	b := p.peek()
	if b != 0 {
		p.pos++
	}
	return b
}

func (p *parser) parseExpr() (float64, error) { return p.parseAddSub() }

func (p *parser) parseAddSub() (float64, error) {
	left, err := p.parseMulDiv()
	if err != nil {
		return 0, err
	}
	for {
		switch p.peek() {
		case '+':
			p.next()
			right, err := p.parseMulDiv()
			if err != nil {
				return 0, err
			}
			left += right
		case '-':
			p.next()
			right, err := p.parseMulDiv()
			if err != nil {
				return 0, err
			}
			left -= right
		default:
			return left, nil
		}
	}
}

func (p *parser) parseMulDiv() (float64, error) {
	left, err := p.parseUnary()
	if err != nil {
		return 0, err
	}
	for {
		switch p.peek() {
		case '*':
			p.next()
			right, err := p.parseUnary()
			if err != nil {
				return 0, err
			}
			left *= right
		case '/':
			p.next()
			right, err := p.parseUnary()
			if err != nil {
				return 0, err
			}
			if right == 0 {
				return 0, fmt.Errorf("division by zero")
			}
			left /= right
		default:
			return left, nil
		}
	}
}

func (p *parser) parseUnary() (float64, error) {
	if p.peek() == '-' {
		p.next()
		v, err := p.parsePrimary()
		if err != nil {
			return 0, err
		}
		return -v, nil
	}
	return p.parsePrimary()
}

func (p *parser) parsePrimary() (float64, error) {
	if p.peek() == '(' {
		p.next()
		v, err := p.parseExpr()
		if err != nil {
			return 0, err
		}
		if p.peek() != ')' {
			return 0, fmt.Errorf("expected ')'")
		}
		p.next()
		return v, nil
	}
	start := p.pos
	for p.pos < len(p.input) && ((p.input[p.pos] >= '0' && p.input[p.pos] <= '9') || p.input[p.pos] == '.') {
		p.pos++
	}
	if start == p.pos {
		return 0, fmt.Errorf("expected number, got %q", string(p.peek()))
	}
	result, err := strconv.ParseFloat(p.input[start:p.pos], 64)
	if err != nil {
		return 0, fmt.Errorf("invalid number %q", p.input[start:p.pos])
	}
	return result, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// requireAPIKey skips the test if OPENAI_API_KEY is not set.
func requireAPIKey(t *testing.T) string {
	t.Helper()
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		t.Skip("OPENAI_API_KEY not set, skipping e2e test")
	}
	return key
}

// newTestAgent creates an agent wired to the real OpenAI provider.
func newTestAgent(t *testing.T, registry *tool.Registry) *Agent {
	t.Helper()
	apiKey := requireAPIKey(t)
	model := os.Getenv("OPENAI_MODEL")
	if model == "" {
		model = "gpt-4o-mini"
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))

	providerOpts := []openai.ProviderOption{openai.WithLogger(logger)}
	if baseURL := os.Getenv("OPENAI_BASE_URL"); baseURL != "" {
		providerOpts = append(providerOpts, openai.WithBaseURL(baseURL))
	}

	provider := openai.NewProvider(apiKey, model, providerOpts...)

	return New(provider, registry,
		WithSystemPrompt("You are a helpful assistant. Use the provided tools when asked to compute something. Be concise."),
		WithLogger(logger),
		WithMaxIter(5),
	)
}

// streamAndLog ranges over the stream iterator, logs each event with elapsed
// time, and returns all collected events and the first error encountered.
func streamAndLog(t *testing.T, seq iter.Seq2[AgentEvent, error]) ([]AgentEvent, error) {
	t.Helper()
	var events []AgentEvent
	start := time.Now()
	for evt, err := range seq {
		elapsed := time.Since(start).Round(time.Millisecond)
		if err != nil {
			t.Logf("[%s] ERROR: %v", elapsed, err)
			return events, err
		}
		switch e := evt.(type) {
		case TextDeltaEvent:
			t.Logf("[%s] TextDelta: %q", elapsed, e.Text)
		case ToolCallEvent:
			t.Logf("[%s] ToolCall: id=%s name=%s args=%s", elapsed, e.ID, e.Name, string(e.Args))
		case ToolResultEvent:
			t.Logf("[%s] ToolResult: id=%s name=%s isError=%v content=%q", elapsed, e.ID, e.Name, e.Result.IsError(), e.Result.Content)
		case ThinkingDeltaEvent:
			t.Logf("[%s] ThinkingDelta: %q", elapsed, e.Text)
		case RetryEvent:
			t.Logf("[%s] Retry: attempt %d/%d, delay %v, reason: %s", elapsed, e.Attempt, e.MaxAttempts, e.Delay, e.Reason)
		case DoneEvent:
			t.Logf("[%s] Done: toolCalls=%d truncated=%v", elapsed, e.ToolCalls, e.Truncated)
		default:
			t.Logf("[%s] Unknown event: %T", elapsed, evt)
		}
		events = append(events, evt)
	}
	return events, nil
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestE2E_RunStream_TextOnly(t *testing.T) {
	registry := tool.NewRegistry()
	// No tools registered — pure text conversation.
	ag := newTestAgent(t, registry)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Use go:linkname-friendly iter import
	seq, err := ag.RunStream(ctx, "What is the capital of France? Answer in one sentence.")
	if err != nil {
		t.Fatalf("RunStream returned error: %v", err)
	}

	events, streamErr := streamAndLog(t, seq)
	if streamErr != nil {
		t.Fatalf("stream error: %v", streamErr)
	}

	if len(events) == 0 {
		t.Fatal("expected at least one event, got zero")
	}

	// Verify: at least one TextDeltaEvent and a final DoneEvent.
	var textCount int
	var doneCount int
	for _, evt := range events {
		switch evt.(type) {
		case TextDeltaEvent:
			textCount++
		case DoneEvent:
			doneCount++
		}
	}

	if textCount == 0 {
		t.Error("expected at least one TextDeltaEvent, got none")
	}
	if doneCount != 1 {
		t.Errorf("expected exactly 1 DoneEvent, got %d", doneCount)
	}

	// The last event must be a DoneEvent.
	if _, ok := events[len(events)-1].(DoneEvent); !ok {
		t.Errorf("expected last event to be DoneEvent, got %T", events[len(events)-1])
	}

	t.Logf("=== Text-only stream: %d events (%d text deltas, %d done) ===", len(events), textCount, doneCount)
}

func TestE2E_RunStream_WithToolCalls(t *testing.T) {
	registry := tool.NewRegistry()
	registry.MustRegister(&calculateTool{})

	ag := newTestAgent(t, registry)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Ask the model to use the calculate tool.
	seq, err := ag.RunStream(ctx, "Please calculate 15 * 37 + 102 for me.")
	if err != nil {
		t.Fatalf("RunStream returned error: %v", err)
	}

	events, streamErr := streamAndLog(t, seq)
	if streamErr != nil {
		t.Fatalf("stream error: %v", streamErr)
	}

	if len(events) == 0 {
		t.Fatal("expected at least one event, got zero")
	}

	// Verify event sequence: ToolCallEvent → ToolResultEvent → TextDeltaEvent → DoneEvent.
	var toolCallCount int
	var toolResultCount int
	var textCount int
	var doneCount int

	for _, evt := range events {
		switch evt.(type) {
		case ToolCallEvent:
			toolCallCount++
		case ToolResultEvent:
			toolResultCount++
		case TextDeltaEvent:
			textCount++
		case DoneEvent:
			doneCount++
		}
	}

	if toolCallCount == 0 {
		t.Error("expected at least one ToolCallEvent (model should use calculate tool)")
	}
	if toolResultCount == 0 {
		t.Error("expected at least one ToolResultEvent")
	}
	if toolCallCount != toolResultCount {
		t.Errorf("ToolCallEvent count (%d) should equal ToolResultEvent count (%d)", toolCallCount, toolResultCount)
	}
	if textCount == 0 {
		t.Error("expected at least one TextDeltaEvent in the final response")
	}
	if doneCount != 1 {
		t.Errorf("expected exactly 1 DoneEvent, got %d", doneCount)
	}

	// Verify the DoneEvent is last.
	last := events[len(events)-1]
	done, ok := last.(DoneEvent)
	if !ok {
		t.Fatalf("expected last event to be DoneEvent, got %T", last)
	}
	if done.Truncated {
		t.Error("agent was truncated — maxIter may be too low or model misbehaved")
	}
	if done.ToolCalls == 0 {
		t.Error("DoneEvent.ToolCalls should be > 0 for a tool-calling interaction")
	}

	t.Logf("=== Tool-call stream: %d events (%d toolCalls, %d toolResults, %d textDeltas, %d done) ===",
		len(events), toolCallCount, toolResultCount, textCount, doneCount)
}
