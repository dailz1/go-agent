package codex

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/llm"
)

func TestStreamContentIndicesAndRefusal(t *testing.T) {
	events := []string{
		`{"type":"response.refusal.delta","output_index":0,"content_index":1,"item_id":"m","delta":"no"}`,
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"item_id":"m","delta":"answer: "}`,
		`{"type":"response.refusal.done","output_index":0,"content_index":1,"item_id":"m","refusal":"no"}`,
		`{"type":"response.output_text.done","output_index":0,"content_index":0,"item_id":"m","text":"answer: "}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"m","role":"assistant","content":[{"type":"output_text","text":"answer: "},{"type":"refusal","refusal":"no"}]}}`,
		`{"type":"response.completed","response":{"status":"completed","output":[{"type":"message","id":"m","role":"assistant","content":[{"type":"output_text","text":"answer: "},{"type":"refusal","refusal":"no"}]}]}}`,
	}
	var text string
	for chunk, err := range scanResponse(t.Context(), strings.NewReader(sse(events...))) {
		if err != nil {
			t.Fatal(err)
		}
		if c, ok := chunk.(llm.TextDeltaChunk); ok {
			text += c.Text
		}
	}
	if text != "answer: no" {
		t.Fatalf("text=%q", text)
	}
}

func TestStreamRejectsContradictions(t *testing.T) {
	for name, events := range map[string][]string{
		"empty delta cannot be replaced": {
			`{"type":"response.output_text.delta","output_index":0,"content_index":0,"item_id":"m","delta":""}`,
			`{"type":"response.completed","response":{"status":"completed","output":[{"type":"message","id":"m","role":"assistant","content":[{"type":"output_text","text":"replacement"}]}]}}`,
		},
		"empty args delta cannot be replaced": {
			`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"f","call_id":"c","name":"f","arguments":""}}`,
			`{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"f","delta":""}`,
			`{"type":"response.completed","response":{"status":"completed","output":[{"type":"function_call","id":"f","call_id":"c","name":"f","arguments":"{}"}]}}`,
		},
		"added unknown content": {
			`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"m","role":"assistant","content":[{"type":"audio","text":"hidden"}]}}`,
			`{"type":"response.completed","response":{"status":"completed","output":[{"type":"message","id":"m","role":"assistant","content":[]}]}}`,
		},
		"late text done": {
			`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"m","role":"assistant","content":[{"type":"output_text","text":"x"}]}}`,
			`{"type":"response.output_text.done","output_index":0,"content_index":1,"item_id":"m","text":"hidden"}`,
			`{"type":"response.completed","response":{"status":"completed","output":[{"type":"message","id":"m","role":"assistant","content":[{"type":"output_text","text":"x"}]}]}}`,
		},
		"final omitted observed item": {
			`{"type":"response.output_text.delta","output_index":1,"content_index":0,"item_id":"m","delta":"x"}`,
			emptyCompletion,
		},
		"duplicate call id": {
			`{"type":"response.completed","response":{"status":"completed","output":[{"type":"function_call","id":"a","call_id":"same","name":"f","arguments":"{}"},{"type":"function_call","id":"b","call_id":"same","name":"f","arguments":"{}"}]}}`,
		},
		"invalid args": {
			`{"type":"response.completed","response":{"status":"completed","output":[{"type":"function_call","id":"a","call_id":"c","name":"f","arguments":"{"}]}}`,
		},
		"changed reasoning": {
			`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"r","encrypted_content":"first","summary":[]}}`,
			`{"type":"response.completed","response":{"status":"completed","output":[{"type":"reasoning","id":"r","encrypted_content":"second","summary":[]}]}}`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			var got error
			for chunk, err := range scanResponse(t.Context(), strings.NewReader(sse(events...))) {
				if _, ok := chunk.(llm.DoneChunk); ok {
					t.Fatal("fake done")
				}
				if err != nil {
					got = err
				}
			}
			var typed *Error
			if !errors.As(got, &typed) || typed.Kind != KindProtocol {
				t.Fatalf("error=%v", got)
			}
		})
	}
}

func TestChatFixtureFoldAndUsage(t *testing.T) {
	fixture, err := os.ReadFile("testdata/two_functions.sse")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(fixture) }))
	defer server.Close()
	p := NewProvider("m", WithAuthSource(&testSource{}), WithBaseURL(server.URL))
	message, usage, err := p.Chat(t.Context(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []llm.ContentBlock{
		llm.ReasoningItemBlock{Type: "reasoning_item", ID: "reasoning_a", EncryptedContent: "synthetic-opaque", Summary: []string{"Use both functions."}},
		llm.ToolUseBlock{Type: "tool_use", ID: "call_a", Name: "first", Input: json.RawMessage(`{"values":[1]}`)},
		llm.ToolUseBlock{Type: "tool_use", ID: "call_b", Name: "second", Input: json.RawMessage(`{"values":[2]}`)},
	}
	if !reflect.DeepEqual(message.Content, want) || !reflect.DeepEqual(usage, &llm.Usage{InputTokens: 20, OutputTokens: 10, ReasoningTokens: 4}) {
		t.Fatalf("message=%+v usage=%+v", message, usage)
	}
}

func TestStreamUsagePresence(t *testing.T) {
	for name, usage := range map[string]string{"missing": "", "null": `,"usage":null`, "zero": `,"usage":{"input_tokens":0,"output_tokens":0}`} {
		t.Run(name, func(t *testing.T) {
			event := `{"type":"response.completed","response":{"status":"completed","output":[]` + usage + `}}`
			for chunk, err := range scanResponse(t.Context(), strings.NewReader(sse(event))) {
				if err != nil {
					t.Fatal(err)
				}
				done := chunk.(llm.DoneChunk)
				if (done.Usage != nil) != (name == "zero") {
					t.Fatalf("usage=%v", done.Usage)
				}
			}
		})
	}
}

func TestStreamCaps(t *testing.T) {
	delta := strings.Repeat("x", 512<<10)
	encoded, err := json.Marshal(delta)
	if err != nil {
		t.Fatal(err)
	}
	event := fmt.Sprintf(`{"type":"response.output_text.delta","output_index":0,"content_index":0,"item_id":"m","delta":%s}`, encoded)
	for name, input := range map[string]string{
		"line":      "data: " + strings.Repeat("x", 1<<20) + "\n\n",
		"item":      strings.Repeat(sse(event), 21),
		"multiline": strings.Repeat("data: "+strings.Repeat(" ", 512<<10)+"\n", 21) + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			var got error
			for _, err := range scanResponse(t.Context(), strings.NewReader(input)) {
				if err != nil {
					got = err
				}
			}
			var typed *Error
			if !errors.As(got, &typed) || typed.Kind != KindProtocol {
				t.Fatalf("error=%v", got)
			}
		})
	}
}

type fragmentReader struct{ reader io.Reader }

func (r fragmentReader) Read(p []byte) (int, error) { return r.reader.Read(p[:1]) }

func TestStreamFramingAndImmediateCompletion(t *testing.T) {
	input := ":comment\n\ndata: {\"type\":\"response.completed\",\ndata: \"response\":{\"status\":\"completed\",\"output\":[]}}\n\ndata: malformed\n\n"
	var count int
	for chunk, err := range scanResponse(t.Context(), fragmentReader{strings.NewReader(input)}) {
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := chunk.(llm.DoneChunk); !ok {
			t.Fatal(chunk)
		}
		count++
	}
	if count != 1 {
		t.Fatalf("done count=%d", count)
	}
}
