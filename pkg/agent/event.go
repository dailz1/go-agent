package agent

import (
	"encoding/json"
	"time"

	"github.com/dailz1/go-agent/pkg/llm"
	"github.com/dailz1/go-agent/pkg/tool"
)

// AgentEvent is a sealed interface for events emitted during agent execution.
// Only the seven types defined in this package (TextDeltaEvent, ThinkingDeltaEvent,
// ToolCallEvent, ToolResultEvent, RetryEvent, DoneEvent, CompactionEvent) can implement it. Consumers should
// use a type switch to handle each variant.
type AgentEvent interface {
	eventType() string
}

// TextDeltaEvent carries an incremental text fragment from the agent's
// streaming response.
type TextDeltaEvent struct {
	// Text is the incremental content produced by the model.
	Text string
}

func (TextDeltaEvent) eventType() string { return "text_delta" }

// ThinkingDeltaEvent carries an incremental reasoning/thinking fragment from
// models that support extended thinking (e.g. DeepSeek Reasoner).
type ThinkingDeltaEvent struct {
	Text string
}

func (ThinkingDeltaEvent) eventType() string { return "thinking_delta" }

// ToolCallEvent announces a tool call the model requested in its reply. All
// calls of an ordinary executable round are announced before any executes.
// A call may never execute because an earlier call fails hard, the run is
// cancelled, or the iteration limit records its deterministic skip result.
// ToolResultEvent reports the recorded outcome. Delivery is synchronous: in
// an ordinary executable round, a consumer that breaks during announcements
// stops the run before any tool executes. An already-closed maxIter terminal
// skip batch is the exception: its results are recorded before its first
// announcement, so a break stops only later event delivery and cannot reopen
// the run for ResumeThread.
type ToolCallEvent struct {
	// ID is the unique identifier for this tool call.
	ID string
	// Name is the function name the agent wants to invoke.
	Name string
	// Args is the complete JSON arguments for the tool call.
	Args json.RawMessage
}

func (ToolCallEvent) eventType() string { return "tool_call" }

// ToolResultEvent is emitted once a call's outcome is recorded in history,
// covering successful execution, soft failures, approval rejections, and
// unknown tools.
type ToolResultEvent struct {
	// ID is the tool call ID this result belongs to.
	ID string
	// Name is the function name the call requested.
	Name string
	// Result is the execution result (may have IsError=true).
	Result *tool.ToolResult
}

func (ToolResultEvent) eventType() string { return "tool_result" }

// RetryInfo carries full details about a single retry attempt.
// It is used both as a callback parameter (via OnRetry) and as a post-hoc
// summary in RunResult.Retries and DoneEvent.Retries.
type RetryInfo struct {
	// Attempt is the total attempt number (1-based). Attempt=1 is first try, Attempt=2 is first retry.
	Attempt int
	// MaxAttempts is the total attempts allowed (e.g., maxRetries+1).
	MaxAttempts int
	// Delay is the duration before this retry attempt.
	Delay time.Duration
	// Reason is a user-friendly, sanitized message (e.g., "rate limited (429)").
	Reason string
	// Err is the original error that triggered this retry.
	Err error
}

// RetryEvent is emitted before each retry attempt in RunStream.
// It is ONLY emitted for pre-stream connection errors. Mid-stream SSE errors are NOT retried.
type RetryEvent struct {
	RetryInfo
}

func (RetryEvent) eventType() string { return "retry" }

// DoneEvent signals that the agent execution has completed. Its fields mirror
// [RunResult] for parity between streaming and non-streaming results.
type DoneEvent struct {
	// Message is the final assistant response.
	Message llm.Message
	// History is the full conversation trace including all intermediate
	// tool calls and results.
	History []llm.Message
	// ToolCalls is the total number of tool calls the model requested
	// in completed rounds (announced calls, including rejected or unknown tools).
	ToolCalls int
	// Truncated is true when the agent hit maxIter without reaching a final
	// text response.
	Truncated bool
	// Retries is the list of all retry attempts that occurred during execution.
	Retries []RetryInfo
	// Usage is the token usage for the last LLM call in this iteration.
	Usage llm.Usage
	// TotalUsage is the cumulative token usage across all iterations.
	TotalUsage llm.Usage
	// ThreadID identifies the persisted thread when a store is configured;
	// empty without one.
	ThreadID string
}

func (DoneEvent) eventType() string { return "done" }

// CompactionEvent is emitted once after the agent compacts conversation
// history to fit the context budget. History is a defensive snapshot of the
// post-compaction conversation.
type CompactionEvent struct {
	Strategies    []string
	DroppedGroups int
	BeforeRunes   int
	AfterRunes    int
	History       []llm.Message
}

func (CompactionEvent) eventType() string { return "compaction" }
