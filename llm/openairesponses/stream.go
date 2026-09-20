package openairesponses

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"net/http"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

// Chat sends a non-streaming Responses request and returns the assistant's
// response assembled from the output items.
func (p *Provider) Chat(ctx context.Context, messages []llm.Message, tools []tool.ToolInfo, opts ...llm.Option) (*llm.Message, *llm.Usage, error) {
	o := llm.ApplyOptions(opts)

	items, instructions, err := convertMessages(messages)
	if err != nil {
		return nil, nil, fmt.Errorf("convert messages: %w", err)
	}
	defs, err := convertToolDefs(tools)
	if err != nil {
		return nil, nil, fmt.Errorf("convert tool definitions: %w", err)
	}

	reqBody, err := buildRequest(o, p.model, items, instructions, defs, false)
	if err != nil {
		return nil, nil, err
	}

	var resp responseEnvelope
	if err := llm.DoJSONRequest(ctx, p.httpClient, llm.RequestConfig{
		Method:  http.MethodPost,
		URL:     p.baseURL + "/responses",
		Headers: p.apiHeaders(),
	}, reqBody, &resp); err != nil {
		p.logger.Error("openairesponses request failed", "error", err)
		return nil, nil, err
	}
	if resp.Error != nil && resp.Error.Message != "" {
		return nil, nil, fmt.Errorf("openairesponses: %s", resp.Error.Message)
	}
	// A response whose status is missing or not "completed" is partial or
	// malformed; returning it as success could terminate an agent with
	// truncated text or incomplete tool arguments.
	if resp.Status != "completed" {
		detail := ""
		if resp.IncompleteDetails != nil {
			detail = " (" + resp.IncompleteDetails.Reason + ")"
		}
		return nil, nil, fmt.Errorf("openairesponses: response status %q%s", resp.Status, detail)
	}

	msg := outputToMessage(p.logger, resp.Output)
	usage := resp.Usage.toUsage()
	return &msg, &usage, nil
}

// ChatStream sends a streaming Responses request and yields semantic events
// translated into the library's chunk model. The request is issued lazily
// inside the iterator, matching the OpenAI adapter's laziness convention (iterator errors are not retried by the agent loop).
func (p *Provider) ChatStream(ctx context.Context, messages []llm.Message, tools []tool.ToolInfo, opts ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	o := llm.ApplyOptions(opts)

	items, instructions, err := convertMessages(messages)
	if err != nil {
		return nil, fmt.Errorf("convert messages: %w", err)
	}
	defs, err := convertToolDefs(tools)
	if err != nil {
		return nil, fmt.Errorf("convert tool definitions: %w", err)
	}

	reqBody, err := buildRequest(o, p.model, items, instructions, defs, true)
	if err != nil {
		return nil, err
	}

	p.logger.Debug("openairesponses stream request sending",
		"model", reqBody.Model,
		"items_count", len(reqBody.Input),
		"tools_count", len(reqBody.Tools),
	)

	return func(yield func(llm.Chunk, error) bool) {
		result, err := llm.DoStreamRequest(ctx, p.httpClient, llm.RequestConfig{
			Method:  http.MethodPost,
			URL:     p.baseURL + "/responses",
			Headers: p.apiHeaders(),
		}, reqBody)
		if err != nil {
			p.logger.Error("openairesponses stream request failed", "error", err)
			yield(nil, err)
			return
		}
		defer result.Cleanup()

		// Reasoning items already delivered via output_item.done; used by the
		// completed handler to avoid duplicate delivery.
		delivered := map[string]bool{}
		yieldedDone := false
		for payload, err := range llm.ScanSSEEvents(ctx, result.Body) {
			if err != nil {
				yield(nil, err)
				return
			}
			chunks, err := p.parseStreamEvent(payload, delivered)
			if err != nil {
				yield(nil, fmt.Errorf("openairesponses: %w", err))
				return
			}
			for _, c := range chunks {
				if _, ok := c.(llm.DoneChunk); ok {
					yieldedDone = true
				}
				if !yield(c, nil) {
					return
				}
			}
		}
		if !yieldedDone {
			yield(nil, fmt.Errorf("openairesponses: stream ended without a completed response"))
		}
	}, nil
}

// streamEvent is the union envelope of Responses SSE events. Only the fields
// the adapter consumes are decoded; unknown event types are ignored.
type streamEvent struct {
	Type        string            `json:"type"`
	OutputIndex int64             `json:"output_index"`
	ItemID      string            `json:"item_id"`
	Delta       string            `json:"delta"`
	Item        *outputItem       `json:"item"`
	Response    *responseEnvelope `json:"response"`
	Message     string            `json:"message"`
}

// parseStreamEvent translates one Responses SSE event into zero or more
// chunks. Unknown event types produce no chunks (forward compatibility).
func (p *Provider) parseStreamEvent(payload string, delivered map[string]bool) ([]llm.Chunk, error) {
	var ev streamEvent
	if err := json.Unmarshal([]byte(payload), &ev); err != nil {
		return nil, fmt.Errorf("decode stream event: %w", err)
	}

	switch ev.Type {
	case "response.output_text.delta":
		if ev.Delta == "" {
			return nil, nil
		}
		return []llm.Chunk{llm.TextDeltaChunk{Text: ev.Delta, OutputIndex: ev.OutputIndex}}, nil

	case "response.reasoning_summary_text.delta":
		if ev.Delta == "" {
			return nil, nil
		}
		return []llm.Chunk{llm.ReasoningDeltaChunk{Text: ev.Delta}}, nil

	case "response.output_item.added":
		if ev.Item == nil || ev.Item.Type != "function_call" {
			return nil, nil
		}
		return []llm.Chunk{llm.ToolCallStartChunk{
			Index: int(ev.OutputIndex),
			ID:    ev.Item.CallID,
			Name:  ev.Item.Name,
		}}, nil

	case "response.function_call_arguments.delta":
		if ev.Delta == "" {
			return nil, nil
		}
		return []llm.Chunk{llm.ToolCallArgsChunk{Index: int(ev.OutputIndex), Delta: ev.Delta}}, nil

	case "response.output_item.done":
		// A finished reasoning item carries its encrypted content; deliver it
		// at its own output position so item order survives assembly.
		if ev.Item == nil || ev.Item.Type != "reasoning" {
			return nil, nil
		}
		delivered[ev.Item.ID] = true
		return []llm.Chunk{llm.ReasoningItemChunk{
			OutputIndex: ev.OutputIndex,
			Item:        reasoningItemOf(*ev.Item),
		}}, nil

	case "response.completed":
		resp := ev.Response
		if resp == nil {
			return nil, fmt.Errorf("completed event without a response payload")
		}
		// Safety net: reasoning items the stream did not already deliver via
		// output_item.done are delivered here, in output order.
		var chunks []llm.Chunk
		for i, item := range resp.Output {
			if item.Type == "reasoning" && !delivered[item.ID] {
				delivered[item.ID] = true
				chunks = append(chunks, llm.ReasoningItemChunk{
					OutputIndex: int64(i),
					Item:        reasoningItemOf(item),
				})
			}
		}
		usage := resp.Usage.toUsage()
		chunks = append(chunks, llm.DoneChunk{FinishReason: "stop", Usage: &usage})
		return chunks, nil

	case "response.failed":
		if ev.Response != nil && ev.Response.Error != nil && ev.Response.Error.Message != "" {
			return nil, fmt.Errorf("response failed: %s", ev.Response.Error.Message)
		}
		return nil, fmt.Errorf("response failed")

	case "response.incomplete":
		reason := ""
		if ev.Response != nil && ev.Response.IncompleteDetails != nil {
			reason = ev.Response.IncompleteDetails.Reason
		}
		return nil, fmt.Errorf("response incomplete: %s", reason)

	case "error":
		return nil, fmt.Errorf("stream error: %s", ev.Message)

	default:
		// Lifecycle and built-in-tool events (content_part.added,
		// web_search_call.*, ...) are ignored for forward compatibility;
		// reasoning output_item.done is handled above.
		return nil, nil
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
