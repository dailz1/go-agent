package llm_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/llm"
)

// TestScanSSEEvents_SingleEvent verifies that a single well-formed SSE event
// is parsed and yielded correctly.
func TestScanSSEEvents_SingleEvent(t *testing.T) {
	t.Parallel()
	input := "data: {\"content\":\"hello\"}\n\n"
	got := collectEvents(t, input)
	if len(got) != 1 {
		t.Fatalf("expected 1 event, got %d", len(got))
	}
	if got[0] != `{"content":"hello"}` {
		t.Errorf("got %q, want %q", got[0], `{"content":"hello"}`)
	}
}

// TestScanSSEEvents_MultipleEvents verifies that multiple consecutive SSE
// events separated by blank lines are all yielded in order.
func TestScanSSEEvents_MultipleEvents(t *testing.T) {
	t.Parallel()
	input := "data: {\"a\":1}\n\ndata: {\"b\":2}\n\n"
	got := collectEvents(t, input)
	if len(got) != 2 {
		t.Fatalf("expected 2 events, got %d", len(got))
	}
	if got[0] != `{"a":1}` {
		t.Errorf("event 0 = %q, want %q", got[0], `{"a":1}`)
	}
	if got[1] != `{"b":2}` {
		t.Errorf("event 1 = %q, want %q", got[1], `{"b":2}`)
	}
}

// TestScanSSEEvents_DoneMarker verifies that the OpenAI "[DONE]" sentinel
// terminates the stream without yielding the marker itself.
func TestScanSSEEvents_DoneMarker(t *testing.T) {
	t.Parallel()
	input := "data: {\"content\":\"hi\"}\n\ndata: [DONE]\n\n"
	got := collectEvents(t, input)
	if len(got) != 1 {
		t.Fatalf("expected 1 event (DONE should terminate), got %d", len(got))
	}
}

// TestScanSSEEvents_SkipsComments verifies that SSE comment lines (starting
// with ":") are silently ignored.
func TestScanSSEEvents_SkipsComments(t *testing.T) {
	t.Parallel()
	input := ": this is a comment\ndata: {\"ok\":true}\n\n"
	got := collectEvents(t, input)
	if len(got) != 1 {
		t.Fatalf("expected 1 event, got %d", len(got))
	}
}

// TestScanSSEEvents_EmptyInput verifies that an empty reader produces zero events.
func TestScanSSEEvents_EmptyInput(t *testing.T) {
	t.Parallel()
	got := collectEvents(t, "")
	if len(got) != 0 {
		t.Errorf("expected 0 events, got %d", len(got))
	}
}

// TestScanSSEEvents_ContextCancellation verifies that a pre-cancelled context
// causes the iterator to yield the context error without reading any events.
func TestScanSSEEvents_ContextCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	input := "data: {\"never\":\"read\"}\n\n"
	var events []string
	for payload, err := range llm.ScanSSEEvents(ctx, strings.NewReader(input)) {
		if err != nil {
			if !errors.Is(err, ctx.Err()) {
				t.Errorf("expected context error, got %v", err)
			}
			break
		}
		events = append(events, payload)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events after cancellation, got %d", len(events))
	}
}

// TestScanSSEEvents_MultilineData verifies that consecutive "data:" lines
// before a blank line are concatenated into a single payload.
func TestScanSSEEvents_MultilineData(t *testing.T) {
	t.Parallel()
	input := "data: {\"line1\":\ndata: \"value\"}\n\n"
	got := collectEvents(t, input)
	if len(got) != 1 {
		t.Fatalf("expected 1 event, got %d", len(got))
	}
	if got[0] != `{"line1":"value"}` {
		t.Errorf("got %q, want %q", got[0], `{"line1":"value"}`)
	}
}

// TestScanSSEEvents_TrailingDataNoBlankLine verifies that a data line without
// a trailing blank line is still yielded at EOF.
func TestScanSSEEvents_TrailingDataNoBlankLine(t *testing.T) {
	t.Parallel()
	got := collectEvents(t, `data: hello`)
	if len(got) != 1 {
		t.Fatalf("expected 1 event, got %d", len(got))
	}
	if got[0] != "hello" {
		t.Errorf("got %q, want %q", got[0], "hello")
	}
}

// TestScanSSEEvents_LastEventNoBlankLine verifies that when the final event
// lacks a trailing blank line, all events are still yielded in order.
func TestScanSSEEvents_LastEventNoBlankLine(t *testing.T) {
	t.Parallel()
	got := collectEvents(t, "data: first\n\ndata: second")
	if len(got) != 2 {
		t.Fatalf("expected 2 events, got %d", len(got))
	}
	if got[0] != "first" {
		t.Errorf("event 0 = %q, want %q", got[0], "first")
	}
	if got[1] != "second" {
		t.Errorf("event 1 = %q, want %q", got[1], "second")
	}
}

// TestScanSSEEvents_TrailingDoneNoBlankLine verifies that a trailing [DONE]
// without blank line still terminates the stream without yielding the marker.
func TestScanSSEEvents_TrailingDoneNoBlankLine(t *testing.T) {
	t.Parallel()
	got := collectEvents(t, "data: {\"ok\":true}\n\ndata: [DONE]")
	if len(got) != 1 {
		t.Fatalf("expected 1 event (DONE should terminate), got %d", len(got))
	}
	if got[0] != `{"ok":true}` {
		t.Errorf("got %q, want %q", got[0], `{"ok":true}`)
	}
}

// TestScanSSEEvents_MultilineDataNoBlankLine verifies that multiline data
// fields without a trailing blank line are concatenated and yielded at EOF.
func TestScanSSEEvents_MultilineDataNoBlankLine(t *testing.T) {
	t.Parallel()
	got := collectEvents(t, "data: {\"a\":\ndata: 1}")
	if len(got) != 1 {
		t.Fatalf("expected 1 event, got %d", len(got))
	}
	if got[0] != `{"a":1}` {
		t.Errorf("got %q, want %q", got[0], `{"a":1}`)
	}
}

// collectEvents is a test helper that drains all SSE events from the given
// input string and returns the collected payloads. It fatals on any
// unexpected error.
func collectEvents(t *testing.T, input string) []string {
	t.Helper()
	var events []string
	for payload, err := range llm.ScanSSEEvents(context.Background(), strings.NewReader(input)) {
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("unexpected error: %v", err)
		}
		events = append(events, payload)
	}
	return events
}
