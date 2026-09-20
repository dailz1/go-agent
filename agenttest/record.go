package agenttest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net"
	"net/url"
	"reflect"
	"sync"
	"syscall"
	"time"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

var (
	ErrActiveRecording       = errors.New("recording has active stream")
	ErrUnsupportedChunk      = errors.New("unsupported recording chunk")
	ErrIncompatibleRecording = errors.New("incompatible recording")
	ErrInterruptedRecording  = errors.New("interrupted recording")
)

// UnsupportedChunkError identifies a pointer-form chunk which cannot be represented by the recording grammar.
type UnsupportedChunkError struct{ Type string }

func (e *UnsupportedChunkError) Error() string { return "unsupported recording chunk: " + e.Type }
func (*UnsupportedChunkError) Unwrap() error   { return ErrUnsupportedChunk }

// Recorder records calls made through its wrapped Provider.
type Recorder struct {
	next        llm.Provider
	mu          sync.Mutex
	exchanges   []dtoExchange
	active      int
	unsupported error
}

func NewRecorder(next llm.Provider) *Recorder { return &Recorder{next: next} }
func (r *Recorder) Name() string              { return "agenttest_recorder(" + r.next.Name() + ")" }

func (r *Recorder) Chat(ctx context.Context, messages []llm.Message, tools []tool.ToolInfo, opts ...llm.Option) (*llm.Message, *llm.Usage, error) {
	index := r.reserve(MethodChat)
	r.setRequest(index, requestDTO(canonicalRequest(messages, tools, opts)))
	response, usage, err := r.next.Chat(ctx, messages, tools, opts...)
	r.mu.Lock()
	r.exchanges[index].Chat = &dtoChat{Response: cloneMessagePtr(response), Usage: cloneUsage(usage), Error: encodeError(err)}
	r.active--
	r.mu.Unlock()
	return response, usage, err
}

func (r *Recorder) ChatStream(ctx context.Context, messages []llm.Message, tools []tool.ToolInfo, opts ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	index := r.reserve(MethodChatStream)
	r.setRequest(index, requestDTO(canonicalRequest(messages, tools, opts)))
	sequence, outerErr := r.next.ChatStream(ctx, messages, tools, opts...)
	if outerErr != nil {
		r.mu.Lock()
		r.exchanges[index].Stream.OuterError = encodeError(outerErr)
		r.exchanges[index].Stream.Completion = "outer_error"
		r.active--
		r.mu.Unlock()
		return nil, outerErr
	}
	return r.recordStream(sequence, index), nil
}

func (r *Recorder) reserve(method Method) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	exchange := dtoExchange{Method: method}
	if method == MethodChatStream {
		exchange.Stream = &dtoStream{}
	}
	r.exchanges = append(r.exchanges, exchange)
	r.active++
	return len(r.exchanges) - 1
}

func (r *Recorder) setRequest(index int, request dtoRequest) {
	r.mu.Lock()
	r.exchanges[index].Request = request
	r.mu.Unlock()
}

func (r *Recorder) recordStream(sequence iter.Seq2[llm.Chunk, error], index int) iter.Seq2[llm.Chunk, error] {
	return func(yield func(llm.Chunk, error) bool) {
		complete := func(status string, streamErr error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.exchanges[index].Stream.Completion != "" {
				return
			}
			r.exchanges[index].Stream.Completion = status
			r.exchanges[index].Stream.StreamError = encodeError(streamErr)
			r.active--
		}
		sequence(func(chunk llm.Chunk, err error) bool {
			if err != nil {
				complete("complete_with_error", err)
				yield(nil, err)
				return false
			}
			if unsupportedChunk(chunk) {
				r.mu.Lock()
				if r.unsupported == nil {
					r.unsupported = &UnsupportedChunkError{Type: reflect.TypeOf(chunk).String()}
				}
				r.mu.Unlock()
			} else {
				encoded, encodeErr := encodeChunk(chunk)
				if encodeErr != nil {
					complete("interrupted", nil)
					return false
				}
				r.mu.Lock()
				r.exchanges[index].Stream.Chunks = append(r.exchanges[index].Stream.Chunks, encoded)
				r.mu.Unlock()
			}
			if !yield(chunk, nil) {
				complete("interrupted", nil)
				return false
			}
			return true
		})
		complete("complete", nil)
	}
}

// Bytes returns a versioned recording after all wrapped streams have completed.
func (r *Recorder) Bytes() ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active != 0 {
		return nil, ErrActiveRecording
	}
	if r.unsupported != nil {
		return nil, r.unsupported
	}
	return json.Marshal(dtoRecording{Version: recordingVersion, Exchanges: append([]dtoExchange(nil), r.exchanges...)})
}

const recordingVersion = 2

type dtoRecording struct {
	Version   int           `json:"version"`
	Exchanges []dtoExchange `json:"exchanges"`
}
type dtoExchange struct {
	Method  Method     `json:"method"`
	Request dtoRequest `json:"request"`
	Chat    *dtoChat   `json:"chat,omitempty"`
	Stream  *dtoStream `json:"stream,omitempty"`
}
type dtoRequest struct {
	Messages []llm.Message   `json:"messages"`
	Tools    []tool.ToolInfo `json:"tools"`
	Options  llm.Options     `json:"options"`
}
type dtoChat struct {
	Response *llm.Message `json:"response"`
	Usage    *llm.Usage   `json:"usage"`
	Error    *dtoError    `json:"error"`
}
type dtoStream struct {
	Chunks      []dtoChunk `json:"chunks"`
	OuterError  *dtoError  `json:"outer_error"`
	StreamError *dtoError  `json:"stream_error"`
	Completion  string     `json:"completion"`
}
type dtoChunk struct {
	Type         string                  `json:"type"`
	Text         string                  `json:"text,omitempty"`
	OutputIndex  int64                   `json:"output_index,omitempty"`
	Index        int                     `json:"index,omitempty"`
	ID           string                  `json:"id,omitempty"`
	Name         string                  `json:"name,omitempty"`
	Delta        string                  `json:"delta,omitempty"`
	Item         *llm.ReasoningItemBlock `json:"item,omitempty"`
	FinishReason string                  `json:"finish_reason,omitempty"`
	Usage        *llm.Usage              `json:"usage"`
}

type dtoError struct {
	Kind       string        `json:"kind"`
	Message    string        `json:"message,omitempty"`
	StatusCode int           `json:"status_code,omitempty"`
	RetryAfter time.Duration `json:"retry_after,omitempty"`
	Body       string        `json:"body,omitempty"`
	Errno      string        `json:"errno,omitempty"`
}

func requestDTO(request Request) dtoRequest {
	return dtoRequest{Messages: cloneMessages(request.Messages), Tools: cloneTools(request.Tools), Options: cloneOptions(request.Options)}
}
func unsupportedChunk(chunk llm.Chunk) bool {
	switch chunk.(type) {
	case *llm.TextDeltaChunk, *llm.ReasoningDeltaChunk, *llm.ToolCallStartChunk, *llm.ToolCallArgsChunk, *llm.ReasoningItemChunk, *llm.DoneChunk:
		return true
	}
	return false
}
func encodeChunk(chunk llm.Chunk) (dtoChunk, error) {
	switch c := chunk.(type) {
	case llm.TextDeltaChunk:
		return dtoChunk{Type: "text_delta", Text: c.Text, OutputIndex: c.OutputIndex}, nil
	case llm.ReasoningDeltaChunk:
		return dtoChunk{Type: "reasoning_delta", Text: c.Text}, nil
	case llm.ToolCallStartChunk:
		return dtoChunk{Type: "tool_call_start", Index: c.Index, ID: c.ID, Name: c.Name}, nil
	case llm.ToolCallArgsChunk:
		return dtoChunk{Type: "tool_call_args", Index: c.Index, ID: c.ID, Delta: c.Delta}, nil
	case llm.ReasoningItemChunk:
		item := c.Item
		item.Summary = append([]string(nil), item.Summary...)
		return dtoChunk{Type: "reasoning_item", OutputIndex: c.OutputIndex, Item: &item}, nil
	case llm.DoneChunk:
		return dtoChunk{Type: "done", FinishReason: c.FinishReason, Usage: cloneUsage(c.Usage)}, nil
	default:
		return dtoChunk{}, fmt.Errorf("unknown chunk %T", chunk)
	}
}

func encodeError(err error) *dtoError {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return &dtoError{Kind: "context_canceled"}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &dtoError{Kind: "deadline_exceeded"}
	}
	if errors.Is(err, llm.ErrStreamingNotSupported) {
		return &dtoError{Kind: "streaming_not_supported"}
	}
	var api *llm.APIError
	if errors.As(err, &api) && api != nil {
		return &dtoError{Kind: "api", StatusCode: api.StatusCode, RetryAfter: api.RetryAfter, Body: api.Body}
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr != nil && llm.IsNetworkError(err) && (netErr.Timeout() || netErr.Temporary()) {
		if netErr.Timeout() {
			return &dtoError{Kind: "network_timeout", Message: err.Error()}
		}
		return &dtoError{Kind: "network_temporary", Message: err.Error()}
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr != nil && llm.IsNetworkError(err) {
		return &dtoError{Kind: "network_url", Message: urlErr.Error()}
	}
	for _, candidate := range []struct {
		err  error
		name string
	}{{syscall.ECONNREFUSED, "ECONNREFUSED"}, {syscall.ECONNRESET, "ECONNRESET"}, {syscall.EPIPE, "EPIPE"}} {
		if errors.Is(err, candidate.err) {
			return &dtoError{Kind: "network_errno", Errno: candidate.name}
		}
	}
	return &dtoError{Kind: "generic", Message: err.Error()}
}
