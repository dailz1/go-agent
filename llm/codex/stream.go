package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"

	"github.com/dailz1/go-agent/llm"
)

func scanResponse(ctx context.Context, body io.Reader) iter.Seq2[llm.Chunk, error] {
	return func(yield func(llm.Chunk, error) bool) {
		state := streamState{items: make(map[int]*itemState), ids: make(map[string]int), calls: make(map[string]int)}
		for payload, err := range llm.ScanSSEEvents(ctx, &eventLimitReader{reader: body}) {
			if err != nil {
				if ctx.Err() != nil {
					yield(nil, ctx.Err())
				} else {
					yield(nil, protocolError("read SSE: %w", err))
				}
				return
			}
			if len(payload) > llm.MaxResponseBody {
				yield(nil, protocolError("event exceeds 10 MiB"))
				return
			}
			var event streamEvent
			if err := json.Unmarshal([]byte(payload), &event); err != nil {
				yield(nil, protocolError("decode SSE: %w", err))
				return
			}
			chunks, done, err := state.event(event)
			if err != nil {
				yield(nil, err)
				return
			}
			for _, chunk := range chunks {
				if !yield(chunk, nil) {
					return
				}
			}
			if done {
				return
			}
		}
		if ctx.Err() != nil {
			yield(nil, ctx.Err())
			return
		}
		yield(nil, protocolError("stream ended without completed response"))
	}
}

// ScanSSEEvents limits lines but not multiline events. Bound their accumulation
// before the shared scanner joins them into a single allocation.
type eventLimitReader struct {
	reader     io.Reader
	eventBytes int
	lineBytes  int
	rawBytes   int
	prefix     [5]byte
	dataLine   bool
}

func (r *eventLimitReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	for i, b := range p[:n] {
		r.rawBytes++
		if r.rawBytes > 2*llm.MaxResponseBody {
			return i, fmt.Errorf("SSE event exceeds 10 MiB")
		}
		switch b {
		case '\n':
			if r.lineBytes == 0 {
				r.eventBytes = 0
				r.rawBytes = 0
			}
			r.lineBytes = 0
			r.dataLine = false
		case '\r':
		default:
			r.lineBytes++
			if r.lineBytes <= len(r.prefix) {
				r.prefix[r.lineBytes-1] = b
				if r.lineBytes == len(r.prefix) {
					r.dataLine = string(r.prefix[:]) == "data:"
				}
			} else if r.dataLine && !(r.lineBytes == 6 && b == ' ') {
				r.eventBytes++
				if r.eventBytes > llm.MaxResponseBody {
					return i, fmt.Errorf("SSE event exceeds 10 MiB")
				}
			}
		}
	}
	return n, err
}
