package agent

import (
	"context"
	"errors"
	"fmt"
	"iter"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

type providerCallResult struct {
	stream        iter.Seq2[llm.Chunk, error]
	err           error
	providerError bool
	stopped       bool
	outboundRunes int
}

func (r *overflowRecovery) chatWithRetryAndFallback(
	ctx context.Context,
	history []llm.Message,
	tools []tool.ToolInfo,
	request *RoundContextRequest,
	yield func(AgentEvent, error) bool,
	streamRetries *[]RetryInfo,
) providerCallResult {
	a := r.a
	maxRetries, baseDelay, maxDelay := a.resolveRetryConfig()
	maxAttempts := maxRetries + 1
	if maxRetries < 0 {
		maxAttempts = 1
	}

	var stream iter.Seq2[llm.Chunk, error]
	var lastRetryErr error
	var outboundRunes int
	prepare := func() ([]llm.Message, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		request.Attempt++
		outbound, err := a.roundContextOutbound(ctx, deepCopyMessages(history), *request, r.window)
		if err != nil {
			return nil, err
		}
		outboundRunes = estimateRunes(outbound)
		if err := r.beforeDispatch(ctx, history, outboundRunes); err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return outbound, nil
	}
	failed := func(err error) providerCallResult {
		return providerCallResult{err: err, providerError: true, outboundRunes: outboundRunes}
	}
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		outbound, err := prepare()
		if err != nil {
			return providerCallResult{err: err}
		}
		stream, lastRetryErr = r.provider.ChatStream(ctx, outbound, tools, r.opts...)
		if err := ctx.Err(); err != nil && lastRetryErr != nil {
			return providerCallResult{err: err}
		}
		if errors.Is(lastRetryErr, llm.ErrContextOverflow) {
			return failed(lastRetryErr)
		}
		if errors.Is(lastRetryErr, llm.ErrStreamingNotSupported) {
			outbound, err = prepare()
			if err != nil {
				return providerCallResult{err: err}
			}
			response, usage, chatErr := r.provider.Chat(ctx, outbound, tools, r.opts...)
			lastRetryErr = chatErr
			if err := ctx.Err(); err != nil {
				return providerCallResult{err: err}
			}
			if errors.Is(chatErr, llm.ErrContextOverflow) {
				return failed(chatErr)
			}
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
			return providerCallResult{err: ctx.Err()}
		}
		if !llm.IsRetryableError(lastRetryErr) && !llm.IsNetworkError(lastRetryErr) {
			return failed(fmt.Errorf("iteration %d: provider chat stream (attempt %d/%d): %w",
				request.Round, attempt, maxAttempts, lastRetryErr))
		}
		a.logger.Warn("agent retrying stream",
			"attempt", attempt+1,
			"max_attempts", maxAttempts,
			"error", lastRetryErr,
		)
		if attempt < maxAttempts {
			delay, rerr := llm.ResolveRetryDelay(ctx, lastRetryErr, baseDelay, maxDelay, attempt)
			if rerr != nil {
				return providerCallResult{err: rerr}
			}
			info := RetryInfo{
				Attempt:     attempt + 1,
				MaxAttempts: maxAttempts,
				Delay:       delay,
				Reason:      llm.SanitizeRetryReason(lastRetryErr),
				Err:         lastRetryErr,
			}
			*streamRetries = append(*streamRetries, info)
			if !yield(RetryEvent{RetryInfo: info}, nil) {
				return providerCallResult{stopped: true}
			}
			if a.retryCfg.OnRetry != nil {
				a.retryCfg.OnRetry(info)
			}
			if werr := llm.SleepFor(ctx, delay); werr != nil {
				return providerCallResult{err: werr}
			}
		}
	}
	if lastRetryErr != nil {
		return failed(fmt.Errorf("iteration %d: provider chat stream failed after %d attempt(s): %w",
			request.Round, maxAttempts, lastRetryErr))
	}
	return providerCallResult{stream: stream, outboundRunes: outboundRunes}
}

func synthesizeChunks(blocks []llm.ContentBlock, usage *llm.Usage) []llm.Chunk {
	chunks := make([]llm.Chunk, 0, len(blocks)+1)
	hasToolUse := false
	for outputIndex, block := range blocks {
		switch b := block.(type) {
		case llm.ReasoningBlock:
			chunks = append(chunks, llm.ReasoningDeltaChunk{Text: b.Content})
		case *llm.ReasoningBlock:
			if b != nil {
				chunks = append(chunks, llm.ReasoningDeltaChunk{Text: b.Content})
			}
		case llm.TextBlock:
			chunks = append(chunks, llm.TextDeltaChunk{Text: b.Text, OutputIndex: int64(outputIndex)})
		case *llm.TextBlock:
			if b != nil {
				chunks = append(chunks, llm.TextDeltaChunk{Text: b.Text, OutputIndex: int64(outputIndex)})
			}
		case llm.ToolUseBlock:
			hasToolUse = true
			chunks = append(chunks,
				llm.ToolCallStartChunk{Index: outputIndex, ID: b.ID, Name: b.Name},
				llm.ToolCallArgsChunk{Index: outputIndex, Delta: string(b.Input)},
			)
		case *llm.ToolUseBlock:
			if b != nil {
				hasToolUse = true
				chunks = append(chunks,
					llm.ToolCallStartChunk{Index: outputIndex, ID: b.ID, Name: b.Name},
					llm.ToolCallArgsChunk{Index: outputIndex, Delta: string(b.Input)},
				)
			}
		case llm.ReasoningItemBlock:
			chunks = append(chunks, llm.ReasoningItemChunk{OutputIndex: int64(outputIndex), Item: b})
		case *llm.ReasoningItemBlock:
			if b != nil {
				chunks = append(chunks, llm.ReasoningItemChunk{OutputIndex: int64(outputIndex), Item: *b})
			}
		case llm.ImageBlock, llm.ToolResultBlock, *llm.ImageBlock, *llm.ToolResultBlock:
		}
	}
	finishReason := "stop"
	if hasToolUse {
		finishReason = "tool_calls"
	}
	return append(chunks, llm.DoneChunk{FinishReason: finishReason, Usage: usage})
}
