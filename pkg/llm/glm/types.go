package glm

import "encoding/json"

type chatRequest struct {
	Model       string         `json:"model"`
	Messages    []chatMessage  `json:"messages"`
	Tools       []toolDef      `json:"tools,omitempty"`
	MaxTokens   int            `json:"max_tokens,omitempty"`
	Temperature *float64         `json:"temperature,omitempty"`
	DoSample    *bool          `json:"do_sample,omitempty"`
	Thinking    *thinkingConfig `json:"thinking,omitempty"`
	// Stream enables SSE streaming when set to true. The response is then
	// consumed via [scanSSEEvents] instead of being parsed as a single JSON
	// object.
	Stream     bool     `json:"stream,omitempty"`
	TopP       *float64 `json:"top_p,omitempty"`
	ToolStream bool     `json:"tool_stream,omitempty"`
	Stop       []string `json:"stop,omitempty"`
	RequestID  string   `json:"request_id,omitempty"`
	UserID     string   `json:"user_id,omitempty"`
}

type chatMessage struct {
	Role       string     `json:"role"`
	Content    any        `json:"content"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type contentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type imageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

type toolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function functionCall `json:"function"`
}

type functionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type toolDef struct {
	Type     string      `json:"type"`
	Function functionDef `json:"function"`
}

type functionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type chatResponse struct {
	ID            string               `json:"id"`
	Model         string               `json:"model"`
	Choices       []choice             `json:"choices"`
	Usage         usageInfo            `json:"usage,omitempty"`
	RequestID     string               `json:"request_id"`
	Created       int64                `json:"created"`
	WebSearch     []webSearchResult    `json:"web_search,omitempty"`
	ContentFilter []contentFilterResult `json:"content_filter,omitempty"`
}

type webSearchResult json.RawMessage

func (r *webSearchResult) UnmarshalJSON(data []byte) error {
	cp := make(json.RawMessage, len(data))
	copy(cp, data)
	*r = webSearchResult(cp)
	return nil
}

type contentFilterResult json.RawMessage

func (r *contentFilterResult) UnmarshalJSON(data []byte) error {
	cp := make(json.RawMessage, len(data))
	copy(cp, data)
	*r = contentFilterResult(cp)
	return nil
}

type choice struct {
	Index        int         `json:"index"`
	Message      respMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type respMessage struct {
	Role             string     `json:"role"`
	Content          *string    `json:"content"`
	ReasoningContent *string    `json:"reasoning_content,omitempty"`
	ToolCalls        []toolCall `json:"tool_calls,omitempty"`
}

// streamResponse represents a single SSE event payload from GLM's streaming
// chat completion API. Each event carries incremental content in the Delta
// field, as opposed to the full Message in [chatResponse].
type streamResponse struct {
	ID      string         `json:"id"`
	Model   string         `json:"model"`
	Choices []streamChoice `json:"choices"`
	Usage   *usageInfo     `json:"usage,omitempty"`
}

type streamChoice struct {
	Index        int         `json:"index"`
	Delta        streamDelta `json:"delta"`
	FinishReason *string     `json:"finish_reason"`
}

// streamDelta carries the incremental content for a single streaming chunk.
// The Content field is a pointer to distinguish absent (nil) from empty ("").
type streamDelta struct {
	Role             string           `json:"role,omitempty"`
	Content          *string          `json:"content,omitempty"`
	ReasoningContent *string          `json:"reasoning_content,omitempty"`
	ToolCalls        []streamToolCall `json:"tool_calls,omitempty"`
}

// streamToolCall represents an incremental tool call fragment within a stream
// delta. On the first chunk of a tool call, ID and Name are populated;
// subsequent chunks carry only Arguments deltas.
type streamToolCall struct {
	Index    int                `json:"index"`
	ID       string             `json:"id,omitempty"`
	Type     string             `json:"type,omitempty"`
	Function streamFunctionCall `json:"function"`
}

// streamFunctionCall carries the function name (on the first chunk) and
// incremental JSON argument fragments within a streaming tool call.
type streamFunctionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type thinkingConfig struct {
	Type          string `json:"type"`
	ClearThinking bool   `json:"clear_thinking,omitempty"`
}

type errorResponse struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type promptTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

type usageInfo struct {
	PromptTokens       int                  `json:"prompt_tokens"`
	CompletionTokens   int                  `json:"completion_tokens"`
	TotalTokens        int                  `json:"total_tokens"`
	PromptTokensDetails *promptTokensDetails `json:"prompt_tokens_details,omitempty"`
}
