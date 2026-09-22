package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/llm"
)

func TestStreamReconciliation(t *testing.T) {
	text := "hello"
	args := `{"n":1}`
	output := []outputItem{
		{Type: "reasoning", ID: "r", EncryptedContent: "opaque", Summary: []summaryPart{{Type: "summary_text", Text: "summary"}}},
		{Type: "message", ID: "m", Role: "assistant", Content: []contentPart{{Type: "output_text", Text: &text}}},
		{Type: "function_call", ID: "f", CallID: "call", Name: "lookup", Arguments: &args},
	}
	completed, err := json.Marshal(streamEvent{Type: "response.completed", Response: &responseEnvelope{Status: "completed", Output: output, Usage: &usageEnvelope{InputTokens: 3, OutputTokens: 4}}})
	if err != nil {
		t.Fatal(err)
	}
	events := []string{
		`{"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","id":"f","call_id":"call","name":"lookup","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","output_index":2,"item_id":"f","delta":"{\"n\":"}`,
		`{"type":"response.output_text.delta","output_index":1,"content_index":0,"item_id":"m","delta":"hello"}`,
		`{"type":"response.function_call_arguments.delta","output_index":2,"item_id":"f","delta":"1}"}`,
		`{"type":"response.function_call_arguments.done","output_index":2,"item_id":"f","arguments":"{\"n\":1}"}`,
		string(completed),
	}
	var chunks []llm.Chunk
	for chunk, err := range scanResponse(context.Background(), strings.NewReader(sse(events...))) {
		if err != nil {
			t.Fatal(err)
		}
		chunks = append(chunks, chunk)
	}
	var gotText, gotArgs string
	var starts, dones, reasoning int
	for _, chunk := range chunks {
		switch c := chunk.(type) {
		case llm.TextDeltaChunk:
			gotText += c.Text
		case llm.ToolCallArgsChunk:
			gotArgs += c.Delta
		case llm.ToolCallStartChunk:
			starts++
		case llm.ReasoningItemChunk:
			reasoning++
			if c.OutputIndex != 0 {
				t.Fatal(c)
			}
		case llm.DoneChunk:
			dones++
			if c.FinishReason != "tool_calls" || !reflect.DeepEqual(c.Usage, &llm.Usage{InputTokens: 3, OutputTokens: 4}) {
				t.Fatal(c)
			}
		}
	}
	if gotText != text || gotArgs != args || starts != 1 || dones != 1 || reasoning != 1 {
		t.Fatalf("%#v", chunks)
	}
}

func TestStreamRejectsInvalidCompletion(t *testing.T) {
	for name, events := range map[string][]string{
		"EOF":                   {},
		"sentinel":              {`[DONE]`},
		"incomplete":            {`{"type":"response.incomplete"}`},
		"failed":                {`{"type":"response.failed"}`},
		"status":                {`{"type":"response.completed","response":{"status":"in_progress","output":[]}}`},
		"unknown":               {`{"type":"response.completed","response":{"status":"completed","output":[{"id":"x","type":"web_search_call"}]}}`},
		"args before start":     {`{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"f","delta":"{}"}`},
		"missing index":         {`{"type":"response.output_text.delta","item_id":"m","content_index":0,"delta":"x"}`},
		"missing content index": {`{"type":"response.output_text.delta","item_id":"m","output_index":0,"delta":"x"}`},
		"missing item id":       {`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"x"}`},
		"text conflict": {
			`{"type":"response.output_text.delta","output_index":0,"content_index":0,"item_id":"m","delta":"x"}`,
			`{"type":"response.completed","response":{"status":"completed","output":[{"type":"message","id":"m","role":"assistant","content":[{"type":"output_text","text":"y"}]}]}}`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			var got error
			for chunk, err := range scanResponse(t.Context(), strings.NewReader(sse(events...))) {
				if _, ok := chunk.(llm.DoneChunk); ok {
					t.Fatal("fake done")
				}
				got = err
			}
			var typed *Error
			if !errors.As(got, &typed) || typed.Kind != KindProtocol {
				t.Fatalf("error=%v", got)
			}
		})
	}
}

func sse(events ...string) string {
	var b strings.Builder
	for _, event := range events {
		fmt.Fprintf(&b, "data: %s\n\n", event)
	}
	return b.String()
}
