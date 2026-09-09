package openai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"iter"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/dailz1/go-agent/pkg/llm"
	"github.com/dailz1/go-agent/pkg/tool"
)

const defaultBaseURL = "https://api.openai.com/v1"

// Provider implements llm.Provider for the OpenAI Chat Completion API.
type Provider struct {
	baseURL    string
	apiKey     string
	model      string
	httpClient *http.Client
	logger     *slog.Logger
}

// ProviderOption configures a Provider via functional options.
type ProviderOption func(*Provider)

// WithBaseURL overrides the default OpenAI API base URL.
// Useful for proxies, Azure OpenAI, or compatible APIs.
func WithBaseURL(url string) ProviderOption {
	return func(p *Provider) { p.baseURL = url }
}

// WithHTTPClient sets a custom HTTP client. Defaults to a client with 120s timeout.
func WithHTTPClient(c *http.Client) ProviderOption {
	return func(p *Provider) { p.httpClient = c }
}

// WithLogger sets the structured logger. Defaults to slog.Default().
func WithLogger(l *slog.Logger) ProviderOption {
	return func(p *Provider) { p.logger = l }
}

// NewProvider creates an OpenAI provider with the given API key and default model.
// The model can be overridden per-call via llm.WithModel.
func NewProvider(apiKey, model string, opts ...ProviderOption) *Provider {
	p := &Provider{
		baseURL: defaultBaseURL,
		apiKey:  apiKey,
		model:   model,
		httpClient: &http.Client{
			Timeout: 120 * time.Second,
		},
		logger: slog.Default(),
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Name returns "openai".
func (p *Provider) Name() string { return "openai" }

// apiHeaders returns the common HTTP headers required for OpenAI API
// authentication. Extracted as a method so Chat and ChatStream share the
// same header construction logic.
func (p *Provider) apiHeaders() map[string]string {
	return map[string]string{
		"Authorization": "Bearer " + p.apiKey,
	}
}

// Chat sends a chat completion request to OpenAI and returns the assistant's response.
//
// Conversion rules:
//   - Internal Message → OpenAI chatMessage:
//     TextBlock → content string/array, ToolUseBlock → tool_calls, ToolResultBlock → role:"tool"
//   - OpenAI response → internal Message:
//     content string → TextBlock, tool_calls → ToolUseBlock
func (p *Provider) Chat(ctx context.Context, messages []llm.Message, tools []tool.ToolInfo, opts ...llm.Option) (*llm.Message, *llm.Usage, error) {
	o := llm.ApplyOptions(opts)

	reqBody := chatRequest{
		Model:       firstNonEmpty(o.Model, p.model),
		MaxTokens:   o.MaxTokens,
		Temperature: o.Temperature,
	}

	reqMessages, err := convertMessages(messages)
	if err != nil {
		return nil, nil, fmt.Errorf("convert messages: %w", err)
	}
	reqBody.Messages = reqMessages

	if len(tools) > 0 {
		defs, err := convertToolDefs(tools)
		if err != nil {
			return nil, nil, fmt.Errorf("convert tool definitions: %w", err)
		}
		reqBody.Tools = defs
	}

	p.logger.Debug("openai request sending",
		"model", reqBody.Model,
		"messages_count", len(reqMessages),
		"tools_count", len(reqBody.Tools),
	)

	var chatResp chatResponse
	err = llm.DoJSONRequest(ctx, p.httpClient, llm.RequestConfig{
		Method:  http.MethodPost,
		URL:     p.baseURL + "/chat/completions",
		Headers: p.apiHeaders(),
	}, reqBody, &chatResp)
	if err != nil {
		p.logger.Error("openai request failed", "error", err)
		return nil, nil, err
	}

	if len(chatResp.Choices) == 0 {
		return nil, nil, fmt.Errorf("openai returned no choices")
	}

	choice := chatResp.Choices[0]
	hasToolCalls := len(choice.Message.ToolCalls) > 0

	p.logger.Debug("openai response received",
		"model", chatResp.Model,
		"finish_reason", choice.FinishReason,
		"has_tool_calls", hasToolCalls,
		"content", llm.Truncate(ptrToString(choice.Message.Content), 500),
	)

	if hasToolCalls {
		for _, tc := range choice.Message.ToolCalls {
			p.logger.Debug("openai tool call",
				"tool_call_id", tc.ID,
				"tool_name", tc.Function.Name,
				"arguments", llm.Truncate(tc.Function.Arguments, 500),
			)
		}
	}

	msg, err := convertResponseMessage(choice.Message)
	if err != nil {
		return nil, nil, fmt.Errorf("convert response: %w", err)
	}
	usage := convertUsage(chatResp.Usage)
	return msg, usage, nil
}

// ChatStream sends a streaming chat completion request to OpenAI and returns
// an iterator that lazily yields [llm.Chunk] values as SSE events arrive.
//
// The caller ranges over the returned iterator to consume chunks. Breaking
// out of the range early is safe — the response body is closed via defer.
//
// On non-2xx HTTP responses, an [*llm.APIError] is returned immediately
// (before the iterator is created). Network-level errors during iteration
// are yielded as errors inside the iterator.
func (p *Provider) ChatStream(ctx context.Context, messages []llm.Message, tools []tool.ToolInfo, opts ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	o := llm.ApplyOptions(opts)

	reqBody := chatRequest{
		Model:         firstNonEmpty(o.Model, p.model),
		MaxTokens:     o.MaxTokens,
		Temperature:   o.Temperature,
		Stream:        true,
		StreamOptions: &streamOptions{IncludeUsage: true},
	}

	reqMessages, err := convertMessages(messages)
	if err != nil {
		return nil, fmt.Errorf("convert messages: %w", err)
	}
	reqBody.Messages = reqMessages

	if len(tools) > 0 {
		defs, err := convertToolDefs(tools)
		if err != nil {
			return nil, fmt.Errorf("convert tool definitions: %w", err)
		}
		reqBody.Tools = defs
	}

	p.logger.Debug("openai stream request sending",
		"model", reqBody.Model,
		"messages_count", len(reqMessages),
		"tools_count", len(reqBody.Tools),
	)

	result, err := llm.DoStreamRequest(ctx, p.httpClient, llm.RequestConfig{
		Method:  http.MethodPost,
		URL:     p.baseURL + "/chat/completions",
		Headers: p.apiHeaders(),
	}, reqBody)
	if err != nil {
		p.logger.Error("openai stream request failed", "error", err)
		return nil, err
	}

	return func(yield func(llm.Chunk, error) bool) {
		defer result.Cleanup()
		for payload, err := range llm.ScanSSEEvents(ctx, result.Body) {
			if err != nil {
				yield(nil, err)
				return
			}
			for _, c := range parseStreamPayload(payload) {
				if !yield(c, nil) {
					return
				}
			}
		}
	}, nil
}

// parseStreamPayload converts a raw SSE JSON payload from OpenAI's streaming
// API into a slice of [llm.Chunk] values. A single SSE event may produce
// multiple chunks (e.g. both a TextDeltaChunk and a DoneChunk when the model
// sends text content alongside a finish_reason). Returns nil on unparseable
// JSON, which the caller should silently skip.
func parseStreamPayload(payload string) []llm.Chunk {
	var resp streamResponse
	if err := json.Unmarshal([]byte(payload), &resp); err != nil {
		return nil
	}

	var chunks []llm.Chunk
	for _, choice := range resp.Choices {
		delta := choice.Delta

		if delta.ReasoningContent != nil && *delta.ReasoningContent != "" {
			chunks = append(chunks, llm.ReasoningDeltaChunk{Text: *delta.ReasoningContent})
		}

		if delta.Content != nil && *delta.Content != "" {
			chunks = append(chunks, llm.TextDeltaChunk{Text: *delta.Content})
		}

		for _, tc := range delta.ToolCalls {
			if tc.ID != "" {
				chunks = append(chunks, llm.ToolCallStartChunk{
					Index: tc.Index,
					ID:    tc.ID,
					Name:  tc.Function.Name,
				})
			}
			if tc.Function.Arguments != "" {
				chunks = append(chunks, llm.ToolCallArgsChunk{
					Index: tc.Index,
					ID:    tc.ID,
					Delta: tc.Function.Arguments,
				})
			}
		}

		if choice.FinishReason != nil && *choice.FinishReason != "" {
			chunks = append(chunks, llm.DoneChunk{
				FinishReason: *choice.FinishReason,
			})
		}
	}

	if resp.Usage != nil {
		chunks = append(chunks, llm.DoneChunk{
			Usage: convertUsage(resp.Usage),
		})
	}

	return chunks
}

func convertMessages(messages []llm.Message) ([]chatMessage, error) {
	result := make([]chatMessage, 0, len(messages))
	for _, m := range messages {
		cm, err := convertMessage(m)
		if err != nil {
			return nil, err
		}
		result = append(result, cm)
	}
	return result, nil
}

func convertMessage(m llm.Message) (chatMessage, error) {
	switch m.Role {
	case llm.RoleTool:
		return convertToolResult(m)
	case llm.RoleAssistant:
		return convertAssistant(m)
	default:
		return convertStandardMessage(m)
	}
}

// convertStandardMessage handles system and user messages.
// Single TextBlock is sent as a plain string for efficiency;
// multiple blocks or images are sent as a content array.
func convertStandardMessage(m llm.Message) (chatMessage, error) {
	cm := chatMessage{Role: string(m.Role)}

	if len(m.Content) == 1 {
		if tb, ok := m.Content[0].(llm.TextBlock); ok {
			cm.Content = tb.Text
			return cm, nil
		}
	}

	parts := make([]contentPart, 0, len(m.Content))
	for _, block := range m.Content {
		switch b := block.(type) {
		case llm.ReasoningBlock:
			continue
		case llm.TextBlock:
			parts = append(parts, contentPart{Type: "text", Text: b.Text})
		case llm.ImageBlock:
			cp := contentPart{Type: "image_url", ImageURL: &imageURL{URL: b.URL}}
			if b.MIMEType != "" && len(b.Data) > 0 {
				cp.ImageURL.URL = "data:" + b.MIMEType + ";base64," + encodeBase64(b.Data)
			}
			parts = append(parts, cp)
		default:
			return chatMessage{}, fmt.Errorf("unsupported content block type %T for role %q", b, m.Role)
		}
	}
	cm.Content = parts
	return cm, nil
}

// convertAssistant extracts TextBlocks → content and ToolUseBlocks → tool_calls.
// ReasoningBlocks are preserved in ReasoningContent for APIs that require them
// (e.g. DeepSeek thinking mode).
func convertAssistant(m llm.Message) (chatMessage, error) {
	cm := chatMessage{Role: "assistant"}

	var textParts []string
	var toolCalls []toolCall
	var reasoningContent *string

	for _, block := range m.Content {
		switch b := block.(type) {
		case llm.ReasoningBlock:
			reasoningContent = &b.Content
			continue
		case llm.TextBlock:
			textParts = append(textParts, b.Text)
		case llm.ToolUseBlock:
			toolCalls = append(toolCalls, toolCall{
				ID:   b.ID,
				Type: "function",
				Function: functionCall{
					Name:      b.Name,
					Arguments: string(b.Input),
				},
			})
		default:
			return chatMessage{}, fmt.Errorf("unsupported content block type %T for assistant message", b)
		}
	}

	if len(textParts) == 1 {
		cm.Content = textParts[0]
	} else if len(textParts) > 1 {
		cm.Content = strings.Join(textParts, "\n")
	} else {
		cm.Content = ""
	}

	cm.ToolCalls = toolCalls
	cm.ReasoningContent = reasoningContent
	return cm, nil
}

// convertToolResult maps a ToolResultBlock to OpenAI's role:"tool" message format.
func convertToolResult(m llm.Message) (chatMessage, error) {
	for _, block := range m.Content {
		if tr, ok := block.(llm.ToolResultBlock); ok {
			return chatMessage{
				Role:       "tool",
				ToolCallID: tr.ToolUseID,
				Content:    tr.Content,
			}, nil
		}
	}
	return chatMessage{}, fmt.Errorf("tool message has no ToolResultBlock")
}

// convertToolDefs maps internal ToolInfo slice to OpenAI function tool definitions.
func convertToolDefs(tools []tool.ToolInfo) ([]toolDef, error) {
	defs := make([]toolDef, len(tools))
	for i, t := range tools {
		params, err := json.Marshal(t.Parameters)
		if err != nil {
			return nil, fmt.Errorf("marshal parameters for tool %q: %w", t.Name, err)
		}
		defs[i] = toolDef{
			Type: "function",
			Function: functionDef{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  params,
			},
		}
	}
	return defs, nil
}

// convertResponseMessage maps an OpenAI response message back to internal Message.
// Text content becomes TextBlock; tool_calls become ToolUseBlock.
func convertResponseMessage(rm respMessage) (*llm.Message, error) {
	var blocks []llm.ContentBlock

	// Add ReasoningBlock BEFORE the empty-check — reasoning-only response is valid
	if rm.ReasoningContent != nil && *rm.ReasoningContent != "" {
		blocks = append(blocks, llm.ReasoningBlock{
			Type:    "reasoning",
			Content: *rm.ReasoningContent,
		})
	}

	if rm.Content != nil && *rm.Content != "" {
		blocks = append(blocks, llm.TextBlock{Type: "text", Text: *rm.Content})
	}

	for _, tc := range rm.ToolCalls {
		blocks = append(blocks, llm.ToolUseBlock{
			Type:  "tool_use",
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: json.RawMessage(tc.Function.Arguments),
		})
	}

	if len(blocks) == 0 {
		return nil, fmt.Errorf("openai returned empty message (no content and no tool_calls)")
	}

	return &llm.Message{
		Role:    llm.RoleAssistant,
		Content: blocks,
	}, nil
}

func convertUsage(u *chatUsage) *llm.Usage {
	if u == nil {
		return &llm.Usage{}
	}
	usage := &llm.Usage{
		InputTokens:  int64(u.PromptTokens),
		OutputTokens: int64(u.CompletionTokens),
	}
	if u.CompletionTokensDetails != nil {
		usage.ReasoningTokens = int64(u.CompletionTokensDetails.ReasoningTokens)
	}
	return usage
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

func encodeBase64(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}

func ptrToString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
