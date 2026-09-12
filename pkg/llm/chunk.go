// Package llm defines provider-agnostic types for LLM interactions, including
// message types, the Provider interface, streaming Chunk types, and shared
// HTTP/retry utilities.
package llm

// Chunk is a sealed interface for streaming response fragments emitted by
// [Provider.ChatStream]. Only the five types defined in this package
// (TextDeltaChunk, ReasoningDeltaChunk, ToolCallStartChunk, ToolCallArgsChunk,
// DoneChunk) can implement it. Consumers should use a type switch to handle
// each variant:
//
//	switch c := chunk.(type) {
//	case llm.TextDeltaChunk:      // append c.Text to output buffer
//	case llm.ReasoningDeltaChunk: // model's thinking/reasoning fragment
//	case llm.ToolCallStartChunk:  // begin a new tool call accumulator
//	case llm.ToolCallArgsChunk:   // append c.Delta to arguments buffer
//	case llm.DoneChunk:           // stream finished (c.FinishReason)
//	}
type Chunk interface {
	chunkType() string
}

// TextDeltaChunk carries an incremental text fragment. Multiple deltas are
// concatenated by the consumer to reconstruct the full assistant text.
type TextDeltaChunk struct {
	// Text is the incremental content produced by the model in this chunk.
	Text string
	// OutputIndex is the position of the owning output item in the
	// provider's output array (Responses protocol); zero for protocols
	// without item indexing.
	OutputIndex int64
}

func (TextDeltaChunk) chunkType() string { return "text_delta" }

// ReasoningDeltaChunk carries an incremental reasoning/thinking fragment from
// models that support extended thinking (e.g. DeepSeek Reasoner). These chunks
// represent the model's internal reasoning process and are emitted before the
// actual response content.
type ReasoningDeltaChunk struct {
	Text string
}

func (ReasoningDeltaChunk) chunkType() string { return "reasoning_delta" }

// ToolCallStartChunk signals the beginning of a tool call. It is emitted once
// per tool call when the model decides to invoke a tool, carrying the call ID
// and function name. Subsequent argument fragments arrive via
// [ToolCallArgsChunk].
type ToolCallStartChunk struct {
	// Index is the positional index of this tool call within the response,
	// matching OpenAI's "tool_calls[].index" field.
	Index int
	// ID is the unique identifier for this tool call (e.g. "call_abc123").
	ID string
	// Name is the function name the model wants to invoke.
	Name string
}

func (ToolCallStartChunk) chunkType() string { return "tool_call_start" }

// ToolCallArgsChunk carries an incremental fragment of the tool call's JSON
// arguments. Consumers accumulate Delta values (keyed by ID) to reconstruct
// the full arguments JSON string.
type ToolCallArgsChunk struct {
	// Index is the positional index of this tool call within the response.
	Index int
	// ID is the unique identifier for this tool call (may be empty on
	// subsequent argument-only deltas).
	ID string
	// Delta is a partial JSON string fragment to append to the accumulated
	// arguments buffer.
	Delta string
}

func (ToolCallArgsChunk) chunkType() string { return "tool_call_args" }

// ReasoningItemChunk delivers one entry-level reasoning item (Responses
// protocol) during streaming, tagged with its output index so the assembled
// assistant message preserves the protocol's item order.
type ReasoningItemChunk struct {
	// OutputIndex is the item's position in the response output array.
	OutputIndex int64
	// Item is the complete reasoning item, including encrypted content when
	// requested.
	Item ReasoningItemBlock
}

func (ReasoningItemChunk) chunkType() string { return "reasoning_item" }

// DoneChunk signals that the stream has completed. No more chunks will follow.
type DoneChunk struct {
	// FinishReason is the stop reason from the provider (e.g. "stop",
	// "tool_calls", "length").
	FinishReason string
	// Usage carries token usage statistics for the completed request.
	// Nil means no usage data is available (e.g. GLM streaming).
	// Non-nil zero value means usage data was reported but all counts are zero.
	Usage *Usage
}

func (DoneChunk) chunkType() string { return "done" }
