package codex

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/llm"
)

func TestChatCompletedWithoutOutputFixture(t *testing.T) {
	fixture, err := os.ReadFile("testdata/completed_without_output.sse")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		data string
	}{
		{name: "empty output", data: string(fixture)},
		{name: "omitted output", data: strings.Replace(string(fixture), `"output":[],"usage"`, `"usage"`, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if _, err := w.Write([]byte(tc.data)); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			provider := NewProvider("m", WithAuthSource(&testSource{}), WithBaseURL(server.URL))
			message, usage, err := provider.Chat(t.Context(), nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			want := llm.AssistantMessage("streamed answer")
			wantUsage := &llm.Usage{InputTokens: 11, OutputTokens: 3, ReasoningTokens: 1}
			if !reflect.DeepEqual(message, &want) || !reflect.DeepEqual(usage, wantUsage) {
				t.Fatalf("message=%+v usage=%+v", message, usage)
			}
			seq, err := provider.ChatStream(t.Context(), nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			var text string
			dones := 0
			for chunk, err := range seq {
				if err != nil {
					t.Fatal(err)
				}
				switch c := chunk.(type) {
				case llm.TextDeltaChunk:
					text += c.Text
				case llm.DoneChunk:
					dones++
					if c.FinishReason != "stop" || !reflect.DeepEqual(c.Usage, wantUsage) {
						t.Fatalf("done=%+v", c)
					}
				}
			}
			if text != "streamed answer" || dones != 1 {
				t.Fatalf("text=%q dones=%d", text, dones)
			}
		})
	}
}

func TestCompletedSparseMixedItems(t *testing.T) {
	prefix := []string{
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"r","encrypted_content":"opaque","summary":[{"type":"summary_text","text":"summary"}]}}`,
		`{"type":"response.refusal.delta","output_index":1,"content_index":1,"item_id":"m","delta":"no"}`,
		`{"type":"response.output_text.delta","output_index":1,"content_index":0,"item_id":"m","delta":"answer: "}`,
		`{"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","id":"f","call_id":"call","name":"lookup","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","output_index":2,"item_id":"f","delta":"[1]"}`,
	}
	for _, tc := range []struct {
		name string
		last string
	}{
		{name: "empty list", last: emptyCompletion},
		{name: "omitted list", last: `{"type":"response.completed","response":{"status":"completed"}}`},
		{name: "subset by id", last: `{"type":"response.completed","response":{"status":"completed","output":[{"type":"message","id":"m","role":"assistant","content":[{"type":"output_text","text":"answer: "}]}]}}`},
		{name: "sparse item fields", last: `{"type":"response.completed","response":{"status":"completed","output":[{"id":"r"},{"id":"m"},{"id":"f"}]}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := sse(prefix...) + sse(tc.last)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if _, err := w.Write([]byte(input)); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			p := NewProvider("m", WithAuthSource(&testSource{}), WithBaseURL(server.URL))
			got, usage, err := p.Chat(t.Context(), nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			want := []llm.ContentBlock{
				llm.ReasoningItemBlock{Type: "reasoning_item", ID: "r", EncryptedContent: "opaque", Summary: []string{"summary"}},
				llm.TextBlock{Type: "text", Text: "answer: no"},
				llm.ToolUseBlock{Type: "tool_use", ID: "call", Name: "lookup", Input: json.RawMessage(`[1]`)},
			}
			if !reflect.DeepEqual(got.Content, want) || usage != nil {
				t.Fatalf("message=%+v usage=%+v", got, usage)
			}
			starts, dones := 0, 0
			for chunk, err := range scanResponse(t.Context(), strings.NewReader(input)) {
				if err != nil {
					t.Fatal(err)
				}
				switch c := chunk.(type) {
				case llm.ToolCallStartChunk:
					starts++
				case llm.DoneChunk:
					dones++
					if c.FinishReason != "tool_calls" {
						t.Fatalf("done=%+v", c)
					}
				}
			}
			if starts != 1 || dones != 1 {
				t.Fatalf("starts=%d dones=%d", starts, dones)
			}
		})
	}
}

func TestCompletedSparseRejectsImpossibleState(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events []string
	}{
		{name: "incomplete function JSON", events: []string{
			`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"f","call_id":"call","name":"lookup","arguments":""}}`,
			`{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"f","delta":"{"}`,
			emptyCompletion,
		}},
		{name: "content index hole", events: []string{
			`{"type":"response.output_text.delta","output_index":0,"content_index":1,"item_id":"m","delta":"lost prefix"}`,
			emptyCompletion,
		}},
		{name: "summary index hole", events: []string{
			`{"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":1,"item_id":"r","delta":"lost prefix"}`,
			emptyCompletion,
		}},
		{name: "duplicate final item", events: []string{
			`{"type":"response.output_text.delta","output_index":0,"content_index":0,"item_id":"m","delta":"x"}`,
			`{"type":"response.completed","response":{"status":"completed","output":[{"id":"m"},{"id":"m"}]}}`,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var failure error
			for chunk, err := range scanResponse(t.Context(), strings.NewReader(sse(tc.events...))) {
				if _, ok := chunk.(llm.DoneChunk); ok {
					t.Fatal("impossible state completed")
				}
				if err != nil {
					failure = err
				}
			}
			var protocol *Error
			if !errors.As(failure, &protocol) || protocol.Kind != KindProtocol {
				t.Fatalf("error=%v", failure)
			}
		})
	}
}

func TestCompletedSparseRetainsOutputIndices(t *testing.T) {
	events := []string{
		`{"type":"response.output_text.delta","output_index":1,"content_index":0,"item_id":"m","delta":"x"}`,
		emptyCompletion,
	}
	var chunks []llm.Chunk
	for chunk, err := range scanResponse(t.Context(), strings.NewReader(sse(events...))) {
		if err != nil {
			t.Fatal(err)
		}
		chunks = append(chunks, chunk)
	}
	want := []llm.Chunk{
		llm.TextDeltaChunk{OutputIndex: 1, Text: "x"},
		llm.DoneChunk{FinishReason: "stop"},
	}
	if !reflect.DeepEqual(chunks, want) {
		t.Fatalf("chunks=%#v", chunks)
	}
}
