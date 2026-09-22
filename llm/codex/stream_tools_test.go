package codex

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/llm"
)

func TestStreamFullArgumentsWithoutDeltas(t *testing.T) {
	for _, arguments := range []string{`{}`, `[]`, `null`, `42`, `"string"`} {
		for _, mode := range []string{"completed only", "arguments done", "item done"} {
			t.Run(arguments+"/"+mode, func(t *testing.T) {
				encoded, err := json.Marshal(arguments)
				if err != nil {
					t.Fatal(err)
				}
				item := fmt.Sprintf(`{"type":"function_call","id":"item","call_id":"call","name":"f","arguments":%s}`, encoded)
				var events []string
				switch mode {
				case "arguments done":
					events = append(events, `{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"item","call_id":"call","name":"f","arguments":""}}`)
					events = append(events, fmt.Sprintf(`{"type":"response.function_call_arguments.done","output_index":0,"item_id":"item","arguments":%s}`, encoded))
				case "item done":
					events = append(events, `{"type":"response.output_item.done","output_index":0,"item":`+item+`}`)
				}
				events = append(events, `{"type":"response.completed","response":{"status":"completed","output":[`+item+`]}}`)
				var got string
				var starts, dones int
				for chunk, err := range scanResponse(t.Context(), strings.NewReader(sse(events...))) {
					if err != nil {
						t.Fatal(err)
					}
					switch c := chunk.(type) {
					case llm.ToolCallStartChunk:
						starts++
						if c.ID != "call" {
							t.Fatal(c)
						}
					case llm.ToolCallArgsChunk:
						got += c.Delta
					case llm.DoneChunk:
						dones++
					}
				}
				if got != arguments || starts != 1 || dones != 1 {
					t.Fatalf("args=%s starts=%d dones=%d", got, starts, dones)
				}
			})
		}
	}
}

func TestStreamArgumentConflictDoesNotComplete(t *testing.T) {
	events := []string{
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"f","call_id":"c","name":"f","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"f","delta":"{}"}`,
		`{"type":"response.function_call_arguments.done","output_index":0,"item_id":"f","arguments":"[]"}`,
	}
	var failed bool
	for chunk, err := range scanResponse(t.Context(), strings.NewReader(sse(events...))) {
		if _, ok := chunk.(llm.DoneChunk); ok {
			t.Fatal("fake done")
		}
		if err != nil {
			failed = true
		}
	}
	if !failed {
		t.Fatal("accepted changed arguments")
	}
}
