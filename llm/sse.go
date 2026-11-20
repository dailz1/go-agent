package llm

import (
	"context"
	"io"
	"iter"

	"github.com/dailz1/go-agent/llm/internal/sse"
)

// ScanSSEEvents reads Server-Sent Events (SSE) from r and lazily yields each
// "data:" field value as a raw string payload. It implements a subset of the
// SSE specification relevant to the OpenAI streaming API:
//
//   - Lines starting with ":" are comments and are skipped.
//   - Consecutive "data:" lines before a blank line are concatenated (multiline
//     data fields).
//   - A blank line dispatches the accumulated data as one event.
//   - The sentinel "data: [DONE]" terminates the stream without yielding.
//   - Context cancellation is checked on every line read. When the reader is
//     provided by [DoStreamRequest], cancellation also unblocks pending reads
//     by closing the underlying HTTP response body.
//
// The returned iterator yields (payload, nil) for each event. On error it
// yields ("", err) and stops. Normal EOF is silent (no error yield).
func ScanSSEEvents(ctx context.Context, r io.Reader) iter.Seq2[string, error] {
	return func(yield func(string, error) bool) {
		for frame, err := range sse.Scan(ctx, r) {
			if !yield(frame.Data, err) || err != nil {
				return
			}
		}
	}
}
