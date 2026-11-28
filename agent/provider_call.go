package agent

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"time"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

type providerCallResult struct {
	measurement   contextMeasurement
	stream        iter.Seq2[llm.Chunk, error]
	err           error
	providerError bool
	stopped       bool
	outboundRunes int
}

type providerRetryState struct {
	attempt     int
	maxAttempts int
	baseDelay   time.Duration
	maxDelay    time.Duration
}

func (a *Agent) newProviderRetryState() providerRetryState {
	n, base, capDelay := a.resolveRetryConfig()
	if n < 0 {
		n = 0
	}
	return providerRetryState{1, n + 1, base, capDelay}
}

func (r *overflowRecovery) retryProvider(
	ctx context.Context, cause error, retry *providerRetryState,
	yield func(AgentEvent, error) bool, retries *[]RetryInfo,
) providerCallResult {
	if err := ctx.Err(); err != nil {
		return providerCallResult{err: err}
	}
	if retry.attempt >= retry.maxAttempts {
		return providerCallResult{err: cause, providerError: true}
	}
	delay, err := llm.ResolveRetryDelay(ctx, cause, retry.baseDelay, retry.maxDelay, retry.attempt)
	if err != nil {
		return providerCallResult{err: err}
	}
	info := RetryInfo{
		Attempt: retry.attempt + 1, MaxAttempts: retry.maxAttempts,
		Delay: delay, Reason: llm.SanitizeRetryReason(cause), Err: cause,
	}
	r.a.logger.Warn("agent retrying stream", "attempt", info.Attempt,
		"max_attempts", info.MaxAttempts, "error", cause)
	*retries = append(*retries, info)
	if !yield(RetryEvent{RetryInfo: info}, nil) {
		return providerCallResult{stopped: true}
	}
	if r.a.retryCfg.OnRetry != nil {
		r.a.retryCfg.OnRetry(info)
	}
	if err := llm.SleepFor(ctx, delay); err != nil {
		return providerCallResult{err: err}
	}
	retry.attempt++
	return providerCallResult{}
}

func (r *overflowRecovery) chatWithRetryAndFallback(
	ctx context.Context,
	history []llm.Message,
	tools []tool.ToolInfo,
	request *RoundContextRequest,
	retry *providerRetryState,
	yield func(AgentEvent, error) bool,
	streamRetries *[]RetryInfo,
) providerCallResult {
	a := r.a

	var stream iter.Seq2[llm.Chunk, error]
	var lastRetryErr error
	var outboundRunes int
	measurement := r.measurement
	if measurement.fingerprintVersion == 0 {
		measurement = requestMeasurement(r.provider, r.opts, tools)
	}
	prepare := func() ([]llm.Message, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		request.Attempt++
		outbound := deepCopyMessages(history)
		var err error
		if a.roundContextProvider != nil {
			var estimated uint64
			var fitter overlayFitter
			if r.context != nil {
				estimated = r.context.estimate(history)
				fitter = r.context.overlayFitter(history)
			} else {
				estimated = uint64(estimateRunes(history))
				fitter = newOverlayFitter(history)
			}
			outbound, err = a.roundContextOutbound(ctx, outbound, *request, r.window, estimated, fitter)
		}
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
	for {
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
				request.Round, retry.attempt, retry.maxAttempts, lastRetryErr))
		}
		next := r.retryProvider(ctx, lastRetryErr, retry, yield, streamRetries)
		if next.stopped || (next.err != nil && !next.providerError) {
			return next
		}
		if next.err != nil {
			return failed(fmt.Errorf("iteration %d: provider chat stream failed after %d attempt(s): %w",
				request.Round, retry.maxAttempts, next.err))
		}
	}
	return providerCallResult{stream: stream, outboundRunes: outboundRunes, measurement: measurement}
}

func synthesizeChunks(blocks []llm.ContentBlock, usage *llm.Usage) []llm.Chunk {
	chunks := make([]llm.Chunk, 0, len(blocks)+1)
	hasToolUse := false
	for outputIndex, block := range blocks {
		switch b := block.(type) {
		case *llm.ReasoningBlock:
			if b != nil {
				chunks = append(chunks, &llm.ReasoningDeltaChunk{Text: b.Content})
			}
		case *llm.TextBlock:
			if b != nil {
				chunks = append(chunks, &llm.TextDeltaChunk{Text: b.Text, OutputIndex: int64(outputIndex)})
			}
		case *llm.ToolUseBlock:
			if b != nil {
				hasToolUse = true
				chunks = append(chunks,
					&llm.ToolCallStartChunk{Index: outputIndex, ID: b.ID, Name: b.Name},
					&llm.ToolCallArgsChunk{Index: outputIndex, Delta: string(b.Input)},
				)
			}
		case *llm.ReasoningItemBlock:
			if b != nil {
				chunks = append(chunks, &llm.ReasoningItemChunk{OutputIndex: int64(outputIndex), Item: *b})
			}
		case *llm.ImageBlock, *llm.ToolResultBlock:
		}
	}
	finishReason := "stop"
	if hasToolUse {
		finishReason = "tool_calls"
	}
	return append(chunks, &llm.DoneChunk{FinishReason: finishReason, Usage: usage})
}
