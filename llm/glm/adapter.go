package glm

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

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

const defaultBaseURL = "https://open.bigmodel.cn/api/paas/v4"

// Provider implements llm.Provider for the GLM Chat Completion API.
type Provider struct {
	baseURL    string
	apiKey     string
	model      string
	httpClient *http.Client
	logger     *slog.Logger
	tokenCache tokenCache
	authMode   string // "bearer" or "jwt"
	thinking   *thinkingConfig
	toolStream bool
	topP       *float64
	doSample   *bool
	requestID  string
	userID     string
}

// ProviderOption configures a Provider via functional options.
type ProviderOption func(*Provider)

// WithBaseURL overrides the default GLM API base URL.
func WithBaseURL(url string) ProviderOption {
	return func(p *Provider) { p.baseURL = url }
}

// WithHTTPClient sets a custom HTTP client.
func WithHTTPClient(c *http.Client) ProviderOption {
	return func(p *Provider) { p.httpClient = c }
}

// WithLogger sets the structured logger.
func WithLogger(l *slog.Logger) ProviderOption {
	return func(p *Provider) { p.logger = l }
}

// WithThinkingEnabled enables GLM thinking/reasoning mode.
func WithThinkingEnabled() ProviderOption {
	return func(p *Provider) {
		p.thinking = &thinkingConfig{Type: "enabled"}
	}
}

// WithClearThinking sets whether to clear thinking content in the response.
func WithClearThinking(clear bool) ProviderOption {
	return func(p *Provider) {
		if p.thinking == nil {
			p.thinking = &thinkingConfig{Type: "enabled"}
		}
		p.thinking.ClearThinking = clear
	}
}

// WithToolStream enables GLM tool_stream mode for streaming tool calls.
func WithToolStream() ProviderOption {
	return func(p *Provider) { p.toolStream = true }
}

// WithTopP sets the top_p sampling parameter. Values outside GLM's accepted
// range are normalized to the nearest boundary in [0.01, 1.0].
func WithTopP(p float64) ProviderOption {
	if p < 0.01 {
		p = 0.01
	} else if p > 1.0 {
		p = 1.0
	}
	return func(provider *Provider) { provider.topP = &p }
}

// WithDoSample sets whether to enable sampling.
func WithDoSample(sample bool) ProviderOption {
	return func(p *Provider) { p.doSample = &sample }
}

// WithRequestID sets a request ID for tracing.
func WithRequestID(id string) ProviderOption {
	return func(p *Provider) { p.requestID = id }
}

// WithUserID sets a user ID for the request.
func WithUserID(id string) ProviderOption {
	return func(p *Provider) { p.userID = id }
}

// NewProvider creates a GLM provider with the given API key and default model.
func NewProvider(apiKey, model string, opts ...ProviderOption) *Provider {
	authMode := "bearer"
	if strings.Contains(apiKey, ".") {
		authMode = "jwt"
	}

	p := &Provider{
		baseURL:  defaultBaseURL,
		apiKey:   apiKey,
		model:    model,
		authMode: authMode,
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

// Name returns "glm".
func (p *Provider) Name() string { return "glm" }

func (p *Provider) apiHeaders() (map[string]string, error) {
	if p.authMode == "jwt" {
		token, err := p.tokenCache.getToken(p.apiKey, 3600)
		if err != nil {
			return nil, fmt.Errorf("generate JWT token: %w", err)
		}
		return map[string]string{
			"Authorization": "Bearer " + token,
		}, nil
	}
	return map[string]string{
		"Authorization": "Bearer " + p.apiKey,
	}, nil
}

// Chat sends a chat completion request to GLM and returns the assistant's response.
func (p *Provider) Chat(ctx context.Context, messages []llm.Message, tools []tool.ToolInfo, opts ...llm.Option) (*llm.Message, *llm.Usage, error) {
	o := llm.ApplyOptions(opts)

	reqBody := chatRequest{
		Model:       firstNonEmpty(o.Model, p.model),
		MaxTokens:   o.MaxTokens,
		Temperature: o.Temperature,
		TopP:        p.topP,
		DoSample:    p.doSample,
		Thinking:    p.thinking,
		RequestID:   p.requestID,
		UserID:      p.userID,
	}

	// Map Stop, truncating to 1 item max (GLM maxItems: 1)
	if len(o.Stop) > 0 {
		if len(o.Stop) > 1 {
			p.logger.Debug("glm stop list truncated", "original", len(o.Stop), "truncated", 1)
		}
		reqBody.Stop = []string{o.Stop[0]}
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

	p.logger.Debug("glm request sending",
		"model", reqBody.Model,
		"messages_count", len(reqMessages),
		"tools_count", len(reqBody.Tools),
	)

	headers, err := p.apiHeaders()
	if err != nil {
		return nil, nil, fmt.Errorf("build auth headers: %w", err)
	}

	var chatResp chatResponse
	err = llm.DoJSONRequest(ctx, p.httpClient, llm.RequestConfig{
		Method:  http.MethodPost,
		URL:     p.baseURL + "/chat/completions",
		Headers: headers,
	}, reqBody, &chatResp)
	if err != nil {
		p.logger.Error("glm request failed", "error", err)
		return nil, nil, err
	}

	if len(chatResp.Choices) == 0 {
		return nil, nil, fmt.Errorf("glm returned no choices")
	}

	choice := chatResp.Choices[0]
	if err := finishReasonError(choice.FinishReason); err != nil {
		return nil, nil, err
	}
	hasToolCalls := len(choice.Message.ToolCalls) > 0

	p.logger.Debug("glm response received",
		"model", chatResp.Model,
		"finish_reason", choice.FinishReason,
		"has_tool_calls", hasToolCalls,
		"content", llm.Truncate(ptrToString(choice.Message.Content), 500),
	)

	if choice.Message.ReasoningContent != nil && *choice.Message.ReasoningContent != "" {
		p.logger.Debug("glm reasoning", "content", llm.Truncate(*choice.Message.ReasoningContent, 500))
	}

	if hasToolCalls {
		for _, tc := range choice.Message.ToolCalls {
			p.logger.Debug("glm tool call",
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

// ChatStream sends a streaming chat completion request to GLM and returns
// an iterator that lazily yields [llm.Chunk] values as SSE events arrive.
func (p *Provider) ChatStream(ctx context.Context, messages []llm.Message, tools []tool.ToolInfo, opts ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	o := llm.ApplyOptions(opts)

	reqBody := chatRequest{
		Model:       firstNonEmpty(o.Model, p.model),
		MaxTokens:   o.MaxTokens,
		Temperature: o.Temperature,
		Stream:      true,
		TopP:        p.topP,
		DoSample:    p.doSample,
		RequestID:   p.requestID,
		UserID:      p.userID,
	}

	// Map Stop, truncating to 1 item max
	if len(o.Stop) > 0 {
		if len(o.Stop) > 1 {
			p.logger.Debug("glm stop list truncated", "original", len(o.Stop), "truncated", 1)
		}
		reqBody.Stop = []string{o.Stop[0]}
	}

	// Set ToolStream when enabled AND stream: true
	if p.toolStream {
		reqBody.ToolStream = true
	}

	// Set Thinking when enabled
	if p.thinking != nil {
		reqBody.Thinking = p.thinking
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

	p.logger.Debug("glm stream request sending",
		"model", reqBody.Model,
		"messages_count", len(reqMessages),
		"tools_count", len(reqBody.Tools),
	)

	headers, err := p.apiHeaders()
	if err != nil {
		return nil, fmt.Errorf("build auth headers: %w", err)
	}

	result, err := llm.DoStreamRequest(ctx, p.httpClient, llm.RequestConfig{
		Method:  http.MethodPost,
		URL:     p.baseURL + "/chat/completions",
		Headers: headers,
	}, reqBody)
	if err != nil {
		p.logger.Error("glm stream request failed", "error", err)
		return nil, err
	}

	return func(yield func(llm.Chunk, error) bool) {
		defer result.Cleanup()
		for payload, err := range llm.ScanSSEEvents(ctx, result.Body) {
			if err != nil {
				yield(nil, err)
				return
			}
			chunks, err := parseStreamPayload(payload)
			if err != nil {
				yield(nil, err)
				return
			}
			for _, c := range chunks {
				if !yield(c, nil) {
					return
				}
			}
		}
	}, nil
}

// parseStreamPayload converts a raw SSE JSON payload from GLM's streaming
// API into a slice of [llm.Chunk] values.
func parseStreamPayload(payload string) ([]llm.Chunk, error) {
	if strings.TrimSpace(payload) == "" {
		return nil, nil
	}

	var resp streamResponse
	if err := json.Unmarshal([]byte(payload), &resp); err != nil {
		return nil, fmt.Errorf("glm: parse stream payload: %w", err)
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
			if err := finishReasonError(*choice.FinishReason); err != nil {
				return nil, err
			}
			dc := llm.DoneChunk{
				FinishReason: *choice.FinishReason,
			}
			if resp.Usage != nil {
				dc.Usage = convertUsage(*resp.Usage)
			}
			chunks = append(chunks, dc)
		}
	}
	if len(resp.Choices) == 0 && resp.Usage != nil {
		chunks = append(chunks, llm.DoneChunk{Usage: convertUsage(*resp.Usage)})
	}
	return chunks, nil
}

func finishReasonError(reason string) error {
	switch reason {
	case "network_error":
		return &llm.APIError{
			StatusCode: http.StatusServiceUnavailable,
			Body:       "glm finish_reason: " + reason,
		}
	case "model_context_window_exceeded":
		return &llm.APIError{
			StatusCode: http.StatusBadRequest,
			Body:       "glm finish_reason: " + reason,
		}
	default:
		return nil
	}
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
		case llm.ReasoningBlock, llm.ReasoningItemBlock, *llm.ReasoningItemBlock:
			// Silently strip — GLM doesn't accept reasoning in outbound messages
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

func convertAssistant(m llm.Message) (chatMessage, error) {
	cm := chatMessage{Role: "assistant"}

	var textParts []string
	var toolCalls []toolCall

	for _, block := range m.Content {
		switch b := block.(type) {
		case llm.ReasoningBlock, llm.ReasoningItemBlock, *llm.ReasoningItemBlock:
			// Silently strip — GLM doesn't accept reasoning in outbound messages
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
	return cm, nil
}

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

// convertResponseMessage maps a GLM response message back to internal Message.
func convertResponseMessage(rm respMessage) (*llm.Message, error) {
	var blocks []llm.ContentBlock

	// Add ReasoningBlock BEFORE the empty-check — thinking-only response is valid
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
		return nil, fmt.Errorf("glm returned empty message (no content and no tool_calls)")
	}

	return &llm.Message{
		Role:    llm.RoleAssistant,
		Content: blocks,
	}, nil
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// convertUsage maps a GLM usageInfo to the shared llm.Usage type.
// GLM uses int for token counts; Usage uses int64.
// GLM never returns ReasoningTokens — always 0.
// Returns non-nil even when API returns zero values.
func convertUsage(u usageInfo) *llm.Usage {
	return &llm.Usage{
		InputTokens:  int64(u.PromptTokens),
		OutputTokens: int64(u.CompletionTokens),
	}
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
