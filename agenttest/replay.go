package agenttest

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"net"
	"net/url"
	"syscall"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/llm/codex"
	"github.com/dailz1/go-agent/llm/codex/auth"
	"github.com/dailz1/go-agent/tool"
)

// Replayer implements llm.Provider from strict v1, v2, and v3 Recorder payloads.
type Replayer struct{ script *ScriptedProvider }

func NewReplayer(recording []byte) (*Replayer, error) {
	data, err := decodeRecording(recording)
	if err != nil {
		return nil, err
	}
	exchanges := make([]Exchange, len(data.Exchanges))
	for i, encoded := range data.Exchanges {
		exchange, err := decodeExchange(encoded)
		if err != nil {
			return nil, fmt.Errorf("recording exchange %d: %w", i, err)
		}
		exchanges[i] = exchange
	}
	return &Replayer{script: NewScriptedProvider(exchanges...)}, nil
}
func (r *Replayer) Name() string { return "agenttest_replayer" }
func (r *Replayer) Chat(ctx context.Context, messages []llm.Message, tools []tool.ToolInfo, opts ...llm.Option) (*llm.Message, *llm.Usage, error) {
	return r.script.Chat(ctx, messages, tools, opts...)
}
func (r *Replayer) ChatStream(ctx context.Context, messages []llm.Message, tools []tool.ToolInfo, opts ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	return r.script.ChatStream(ctx, messages, tools, opts...)
}
func (r *Replayer) Verify() error { return r.script.Verify() }
func incompatiblef(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrIncompatibleRecording}, args...)...)
}

func decodeExchange(encoded dtoExchange) (Exchange, error) {
	request := Request{Messages: cloneMessages(encoded.Request.Messages), Tools: cloneTools(encoded.Request.Tools), Options: cloneOptions(encoded.Request.Options)}
	switch encoded.Method {
	case MethodChat:
		if encoded.Chat == nil || encoded.Stream != nil {
			return Exchange{}, incompatiblef("invalid chat exchange")
		}
		err, decodeErr := decodeError(encoded.Chat.Error)
		if decodeErr != nil {
			return Exchange{}, decodeErr
		}
		return Exchange{Method: MethodChat, Request: request, ChatResponse: cloneMessagePtr(encoded.Chat.Response), ChatUsage: cloneUsage(encoded.Chat.Usage), ChatErr: err}, nil
	case MethodChatStream:
		if encoded.Stream == nil || encoded.Chat != nil {
			return Exchange{}, incompatiblef("invalid stream exchange")
		}
		stream := encoded.Stream
		outerErr, err := decodeError(stream.OuterError)
		if err != nil {
			return Exchange{}, err
		}
		streamErr, err := decodeError(stream.StreamError)
		if err != nil {
			return Exchange{}, err
		}
		if stream.Completion == "interrupted" {
			return Exchange{}, ErrInterruptedRecording
		}
		if outerErr != nil {
			if stream.Completion != "outer_error" || streamErr != nil || len(stream.Chunks) != 0 {
				return Exchange{}, incompatiblef("invalid outer-error stream")
			}
			return Exchange{Method: MethodChatStream, Request: request, StreamOuterErr: outerErr}, nil
		}
		if stream.Completion != "complete" && stream.Completion != "complete_with_error" {
			return Exchange{}, incompatiblef("invalid stream completion %q", stream.Completion)
		}
		if (stream.Completion == "complete") != (streamErr == nil) {
			return Exchange{}, incompatiblef("inconsistent stream error completion")
		}
		chunks := make([]llm.Chunk, len(stream.Chunks))
		for i, item := range stream.Chunks {
			chunk, err := decodeChunk(item)
			if err != nil {
				return Exchange{}, fmt.Errorf("chunk %d: %w", i, err)
			}
			chunks[i] = chunk
		}
		return Exchange{Method: MethodChatStream, Request: request, StreamChunks: chunks, StreamErr: streamErr}, nil
	default:
		return Exchange{}, incompatiblef("unknown method %q", encoded.Method)
	}
}
func decodeChunk(encoded dtoChunk) (llm.Chunk, error) {
	switch encoded.Type {
	case "text_delta":
		return llm.TextDeltaChunk{Text: encoded.Text, OutputIndex: encoded.OutputIndex}, nil
	case "reasoning_delta":
		return llm.ReasoningDeltaChunk{Text: encoded.Text}, nil
	case "tool_call_start":
		return llm.ToolCallStartChunk{Index: encoded.Index, ID: encoded.ID, Name: encoded.Name}, nil
	case "tool_call_args":
		return llm.ToolCallArgsChunk{Index: encoded.Index, ID: encoded.ID, Delta: encoded.Delta}, nil
	case "reasoning_item":
		if encoded.Item == nil {
			return nil, incompatiblef("reasoning item missing item")
		}
		item := *encoded.Item
		item.Summary = cloneStrings(item.Summary)
		return llm.ReasoningItemChunk{OutputIndex: encoded.OutputIndex, Item: item}, nil
	case "done":
		return llm.DoneChunk{FinishReason: encoded.FinishReason, Usage: cloneUsage(encoded.Usage)}, nil
	default:
		return nil, incompatiblef("unknown chunk type %q", encoded.Type)
	}
}
func decodeError(encoded *dtoError) (error, error) {
	if encoded == nil {
		return nil, nil
	}
	switch encoded.Kind {
	case "context_canceled":
		return context.Canceled, nil
	case "deadline_exceeded":
		return context.DeadlineExceeded, nil
	case "streaming_not_supported":
		return llm.ErrStreamingNotSupported, nil
	case "api":
		return &llm.APIError{StatusCode: encoded.StatusCode, RetryAfter: encoded.RetryAfter, Body: encoded.Body, NonRetryable: encoded.NonRetryable}, nil
	case "codex", "codex_auth":
		cause, err := decodeError(encoded.Cause)
		if err != nil {
			return nil, err
		}
		if encoded.Kind == "codex" {
			return &codex.Error{Kind: encoded.Category, Code: encoded.Code, RetryAt: encoded.RetryAt, Cause: cause}, nil
		}
		return &auth.Error{Stage: encoded.Stage, Code: encoded.Code, Temporary: encoded.Temporary, LoginRequired: encoded.LoginRequired, Message: encoded.Message, Cause: cause}, nil
	case "network_timeout":
		return replayNetError{message: encoded.Message, timeout: true}, nil
	case "network_temporary":
		return replayNetError{message: encoded.Message, temporary: true}, nil
	case "network_url":
		return &url.Error{Err: replayNetError{message: encoded.Message, temporary: true}}, nil
	case "network_errno":
		switch encoded.Errno {
		case "ECONNREFUSED":
			return syscall.ECONNREFUSED, nil
		case "ECONNRESET":
			return syscall.ECONNRESET, nil
		case "EPIPE":
			return syscall.EPIPE, nil
		default:
			return nil, incompatiblef("unknown network errno %q", encoded.Errno)
		}
	case "generic":
		return errors.New(encoded.Message), nil
	default:
		return nil, incompatiblef("unknown error type %q", encoded.Kind)
	}
}

type replayNetError struct {
	message            string
	timeout, temporary bool
}

func (e replayNetError) Error() string   { return e.message }
func (e replayNetError) Timeout() bool   { return e.timeout }
func (e replayNetError) Temporary() bool { return e.temporary }

var _ net.Error = replayNetError{}
