package agent

import (
	"context"
	"errors"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

// receiveRound owns the request cycles of one logical round. It never returns
// a failed attempt's accumulator, drains steering, or refreshes frozen tools.
func (r *overflowRecovery) receiveRound(
	ctx context.Context,
	history *[]llm.Message,
	tools []tool.ToolInfo,
	request RoundContextRequest,
	yield func(AgentEvent, error) bool,
	retries *[]RetryInfo,
) (*toolCallAccum, bool) {
	for {
		if !r.prepare(ctx, history, yield) {
			return nil, false
		}
		call := r.chatWithRetryAndFallback(ctx, *history, tools, &request, yield, retries)
		if call.stopped {
			return nil, false
		}
		if call.err != nil && !call.providerError {
			yield(nil, call.err)
			return nil, false
		}
		accum := &toolCallAccum{}
		started := false
		if call.err == nil {
			for chunk, chunkErr := range call.stream {
				// Interface presence, not recognized chunk type or visible text,
				// decides whether the caller has already received a partial stream.
				started = started || chunk != nil
				if err := ctx.Err(); err != nil {
					call.err = err
					break
				}
				if chunkErr != nil {
					call.err, call.providerError = chunkErr, true
					break
				}
				accum.feed(chunk)
				var event AgentEvent
				switch c := chunk.(type) {
				case *llm.ReasoningDeltaChunk:
					if c != nil {
						event = ThinkingDeltaEvent{Text: c.Text}
					}
				case *llm.TextDeltaChunk:
					if c != nil {
						event = TextDeltaEvent{Text: c.Text}
					}
				}
				if event != nil && !yield(event, nil) {
					return nil, false
				}
			}
		}
		// The range and every synchronous wrapper have returned here. In
		// particular their defers release permits before a summarizer can run.
		if err := ctx.Err(); err != nil {
			yield(nil, err)
			return nil, false
		}
		if call.err == nil {
			r.measurement = call.measurement
			return accum, true
		}
		if !errors.Is(call.err, llm.ErrContextOverflow) {
			yield(nil, r.providerFailed(ctx, *history, call.err))
			return nil, false
		}
		if err := r.rejected(ctx, *history, call.outboundRunes, call.err); err != nil {
			if cancel := ctx.Err(); cancel != nil {
				yield(nil, cancel)
			} else {
				yield(nil, r.failure("persistence_failed", err))
			}
			return nil, false
		}
		if !r.a.overflowRecovery {
			yield(nil, r.failure("disabled", nil))
			return nil, false
		}
		if started {
			yield(nil, call.err)
			return nil, false
		}
	}
}
