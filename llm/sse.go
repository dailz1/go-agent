package llm

import (
	"bufio"
	"context"
	"io"
	"iter"
	"strings"
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
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		var dataLines []string

		for scanner.Scan() {
			select {
			case <-ctx.Done():
				yield("", ctx.Err())
				return
			default:
			}

			line := scanner.Text()

			if strings.HasPrefix(line, ":") {
				continue
			}

			if line == "" {
				if len(dataLines) > 0 {
					payload := strings.Join(dataLines, "")
					dataLines = dataLines[:0]

					if payload == "[DONE]" {
						return
					}

					if !yield(payload, nil) {
						return
					}
				}
				continue
			}

			if after, ok := strings.CutPrefix(line, "data: "); ok {
				dataLines = append(dataLines, after)
			} else if after, ok := strings.CutPrefix(line, "data:"); ok {
				dataLines = append(dataLines, after)
			}
		}

		// Flush any remaining data that wasn't terminated by a blank line.
		// Per SSE spec, the last event should be dispatched even without a
		// trailing blank line. This also handles connection drops mid-event.
		if len(dataLines) > 0 {
			payload := strings.Join(dataLines, "")
			dataLines = dataLines[:0]

			if payload == "[DONE]" {
				return
			}

			if !yield(payload, nil) {
				return
			}
		}

		if err := scanner.Err(); err != nil {
			if ctx.Err() != nil {
				yield("", ctx.Err())
			} else {
				yield("", err)
			}
		}
	}
}
