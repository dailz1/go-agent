package codex

import (
	"fmt"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/llm"
)

func TestStreamReasoningSummaryLifecycle(t *testing.T) {
	events := []string{
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"r","summary":[]}}`,
		`{"type":"response.reasoning_summary_part.added","output_index":0,"summary_index":0,"item_id":"r","part":{"type":"summary_text","text":""}}`,
		`{"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":0,"item_id":"r","delta":"summary"}`,
		`{"type":"response.reasoning_summary_text.done","output_index":0,"summary_index":0,"item_id":"r","text":"summary"}`,
		`{"type":"response.reasoning_summary_part.done","output_index":0,"summary_index":0,"item_id":"r","part":{"type":"summary_text","text":"summary"}}`,
		`{"type":"response.completed","response":{"status":"completed","output":[{"type":"reasoning","id":"r","encrypted_content":"opaque","summary":[{"type":"summary_text","text":"summary"}]}]}}`,
	}
	var text string
	var reasoning int
	for chunk, err := range scanResponse(t.Context(), strings.NewReader(sse(events...))) {
		if err != nil {
			t.Fatal(err)
		}
		switch c := chunk.(type) {
		case llm.ReasoningDeltaChunk:
			text += c.Text
		case llm.ReasoningItemChunk:
			reasoning++
		}
	}
	if text != "summary" || reasoning != 1 {
		t.Fatalf("text=%q items=%d", text, reasoning)
	}
}

func TestStreamCompletedPayloadAtLimit(t *testing.T) {
	prefix := `{"type":"response.completed","response":{"status":"completed","output":[{"type":"message","id":"m","role":"assistant","content":[{"type":"output_text","text":"`
	suffix := `"}]}]}}`
	payload := prefix + strings.Repeat("x", llm.MaxResponseBody-len(prefix)-len(suffix)) + suffix
	var input strings.Builder
	for len(payload) > 0 {
		n := min(len(payload), 512<<10)
		fmt.Fprintf(&input, "data: %s\n", payload[:n])
		payload = payload[n:]
	}
	input.WriteByte('\n')
	var done bool
	for chunk, err := range scanResponse(t.Context(), strings.NewReader(input.String())) {
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := chunk.(llm.DoneChunk); ok {
			done = true
		}
	}
	if !done {
		t.Fatal("missing completed response")
	}
}

func TestStreamSummaryFinalOnlyAfterAdded(t *testing.T) {
	input := sse(
		`{"type":"response.reasoning_summary_part.added","output_index":0,"summary_index":0,"item_id":"r","part":{"type":"summary_text","text":""}}`,
		`{"type":"response.completed","response":{"status":"completed","output":[{"type":"reasoning","id":"r","encrypted_content":"opaque","summary":[{"type":"summary_text","text":"complete"}]}]}}`,
	)
	for _, err := range scanResponse(t.Context(), strings.NewReader(input)) {
		if err != nil {
			t.Fatal(err)
		}
	}
}
