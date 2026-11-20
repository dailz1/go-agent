// Package sse retains frame boundaries for provider error classification.
package sse

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"iter"
	"strings"
)

type Frame struct {
	Data     string
	complete bool
	joined   string
}

// CompleteJSON excludes EOF-flushed frames and JSON made valid only by the
// legacy removal of newlines between consecutive data fields.
func (f Frame) CompleteJSON() bool {
	return f.complete && json.Valid([]byte(f.joined))
}

func Scan(ctx context.Context, r io.Reader) iter.Seq2[Frame, error] {
	return func(yield func(Frame, error) bool) {
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		var dataLines []string
		for scanner.Scan() {
			if err := ctx.Err(); err != nil {
				yield(Frame{}, err)
				return
			}
			line := scanner.Text()
			if strings.HasPrefix(line, ":") {
				continue
			}
			if line == "" {
				if len(dataLines) > 0 {
					frame := Frame{Data: strings.Join(dataLines, ""), complete: true, joined: strings.Join(dataLines, "\n")}
					dataLines = dataLines[:0]
					if frame.Data == "[DONE]" || !yield(frame, nil) {
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
		if err := scanner.Err(); err != nil {
			if ctx.Err() != nil {
				err = ctx.Err()
			}
			yield(Frame{}, err)
			return
		}
		if len(dataLines) > 0 {
			payload := strings.Join(dataLines, "")
			if payload != "[DONE]" {
				yield(Frame{Data: payload}, nil)
			}
		}
	}
}
