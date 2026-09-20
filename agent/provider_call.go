package agent

import (
	"context"
	"errors"
	"fmt"
	"iter"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

func (a *Agent) chatWithRetryAndFallback(
	ctx context.Context,
	history []llm.Message,
	tools []tool.ToolInfo,
	request RoundContextRequest,
	yield func(AgentEvent, error) bool,
	streamRetries *[]RetryInfo,
) (iter.Seq2[llm.Chunk, error], bool) {
	maxRetries, baseDelay, maxDelay := a.resolveRetryConfig()
	maxAttempts := maxRetries + 1
	if maxRetries < 0 {
		maxAttempts = 1
	}

	var stream iter.Seq2[llm.Chunk, error]
	var lastRetryErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		request.Attempt++
		outbound, err := a.roundContextOutbound(ctx, history, request)
		if err != nil {
			yield(nil, err)
			return nil, false
		}
		stream, lastRetryErr = a.provider.ChatStream(ctx, outbound, tools, a.llmOpts...)
		if errors.Is(lastRetryErr, llm.ErrStreamingNotSupported) {
			request.Attempt++
			outbound, err = a.roundContextOutbound(ctx, history, request)
			if err != nil {
				yield(nil, err)
				return nil, false
			}
			response, usage, chatErr := a.provider.Chat(ctx, outbound, tools, a.llmOpts...)
			lastRetryErr = chatErr
			if chatErr == nil {
				stream = func(yield func(llm.Chunk, error) bool) {
					for _, chunk := range synthesizeChunks(response.Content, usage) {
						if !yield(chunk, nil) {
							return
						}
					}
				}
				break
			}
		}
		if lastRetryErr == nil {
			break
		}
		if ctx.Err() != nil {
			yield(nil, ctx.Err())
			return nil, false
		}
		if !llm.IsRetryableError(lastRetryErr) && !llm.IsNetworkError(lastRetryErr) {
			yield(nil, fmt.Errorf("iteration %d: provider chat stream (attempt %d/%d): %w", request.Round, attempt, maxAttempts, lastRetryErr))
			return nil, false
		}
		a.logger.Warn("agent retrying stream",
			"attempt", attempt+1,
			"max_attempts", maxAttempts,
			"error", lastRetryErr,
		)
		if attempt < maxAttempts {
			delay := llm.Backoff(baseDelay, maxDelay, attempt)
			info := RetryInfo{
				Attempt:     attempt + 1,
				MaxAttempts: maxAttempts,
				Delay:       delay,
				Reason:      llm.SanitizeRetryReason(lastRetryErr),
				Err:         lastRetryErr,
			}
			*streamRetries = append(*streamRetries, info)
			if !yield(RetryEvent{RetryInfo: info}, nil) {
				return nil, false
			}
			if a.retryCfg.OnRetry != nil {
				a.retryCfg.OnRetry(info)
			}
			if werr := llm.WaitForRetry(ctx, lastRetryErr, baseDelay, maxDelay, attempt); werr != nil {
				yield(nil, werr)
				return nil, false
			}
		}
	}
	if lastRetryErr != nil {
		yield(nil, fmt.Errorf("iteration %d: provider chat stream failed after %d attempt(s): %w", request.Round, maxAttempts, lastRetryErr))
		return nil, false
	}
	return stream, true
}

func synthesizeChunks(blocks []llm.ContentBlock, usage *llm.Usage) []llm.Chunk {
	chunks := make([]llm.Chunk, 0, len(blocks)+1)
	hasToolUse := false
	for outputIndex, block := range blocks {
		switch b := block.(type) {
		case llm.ReasoningBlock:
			chunks = append(chunks, llm.ReasoningDeltaChunk{Text: b.Content})
		case llm.TextBlock:
			chunks = append(chunks, llm.TextDeltaChunk{Text: b.Text, OutputIndex: int64(outputIndex)})
		case llm.ToolUseBlock:
			hasToolUse = true
			chunks = append(chunks,
				llm.ToolCallStartChunk{Index: outputIndex, ID: b.ID, Name: b.Name},
				llm.ToolCallArgsChunk{Index: outputIndex, Delta: string(b.Input)},
			)
		case llm.ReasoningItemBlock:
			chunks = append(chunks, llm.ReasoningItemChunk{OutputIndex: int64(outputIndex), Item: b})
		case llm.ImageBlock, llm.ToolResultBlock:
		}
	}
	finishReason := "stop"
	if hasToolUse {
		finishReason = "tool_calls"
	}
	return append(chunks, llm.DoneChunk{FinishReason: finishReason, Usage: usage})
}
