package agenttest

import (
	"context"
	"errors"
	"testing"

	"github.com/dailz1/go-agent/llm"
)

const validWireRequest = `{"messages":[],"tools":null,"options":{"Model":"","MaxTokens":0,"Temperature":null,"Stop":null}}`

func TestReplayerRejectsStrictWireViolations(t *testing.T) {
	cases := []struct{ name, recording string }{
		{"unknown top-level", `{"version":1,"exchanges":[],"extra":true}`},
		{"unknown nested field", `{"version":1,"exchanges":[{"method":"chat","request":{"messages":[{"role":"user","content":[{"type":"text","text":"x","extra":true}]}],"tools":null,"options":{"Model":"","MaxTokens":0,"Temperature":null,"Stop":null}},"chat":{"response":null,"usage":null,"error":null}}]}`},
		{"wrong typed content", `{"version":1,"exchanges":[{"method":"chat","request":{"messages":[{"role":"user","content":[{"type":"text","text":1}]}],"tools":null,"options":{"Model":"","MaxTokens":0,"Temperature":null,"Stop":null}},"chat":{"response":null,"usage":null,"error":null}}]}`},
		{"missing request", `{"version":1,"exchanges":[{"method":"chat","chat":{"response":null,"usage":null,"error":null}}]}`},
		{"missing response", `{"version":1,"exchanges":[{"method":"chat","request":` + validWireRequest + `,"chat":{"usage":null,"error":null}}]}`},
		{"missing stream error", streamWire(`[]`, `"stream_error":null,"completion":"complete"`)},
		{"text delta", streamWire(`[{"type":"text_delta","output_index":0}]`, `"outer_error":null,"stream_error":null,"completion":"complete"`)},
		{"reasoning delta", streamWire(`[{"type":"reasoning_delta"}]`, `"outer_error":null,"stream_error":null,"completion":"complete"`)},
		{"tool start", streamWire(`[{"type":"tool_call_start","index":0,"id":"id"}]`, `"outer_error":null,"stream_error":null,"completion":"complete"`)},
		{"tool args", streamWire(`[{"type":"tool_call_args","index":0,"id":"id"}]`, `"outer_error":null,"stream_error":null,"completion":"complete"`)},
		{"reasoning item", streamWire(`[{"type":"reasoning_item","output_index":0}]`, `"outer_error":null,"stream_error":null,"completion":"complete"`)},
		{"reasoning item missing nested type", streamWire(`[{"type":"reasoning_item","output_index":0,"item":{"id":"item"}}]`, `"outer_error":null,"stream_error":null,"completion":"complete"`)},
		{"reasoning item unknown nested type", streamWire(`[{"type":"reasoning_item","output_index":0,"item":{"type":"unknown"}}]`, `"outer_error":null,"stream_error":null,"completion":"complete"`)},
		{"done", streamWire(`[{"type":"done","finish_reason":"stop"}]`, `"outer_error":null,"stream_error":null,"completion":"complete"`)},
		{"illegal chunk field", streamWire(`[{"type":"text_delta","text":"x","output_index":0,"id":"forbidden"}]`, `"outer_error":null,"stream_error":null,"completion":"complete"`)},
		{"missing error message", `{"version":1,"exchanges":[{"method":"chat","request":` + validWireRequest + `,"chat":{"response":null,"usage":null,"error":{"kind":"generic"}}}]}`},
		{"illegal error field", `{"version":1,"exchanges":[{"method":"chat","request":` + validWireRequest + `,"chat":{"response":null,"usage":null,"error":{"kind":"generic","message":"x","status_code":1}}}]}`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewReplayer([]byte(test.recording)); !errors.Is(err, ErrIncompatibleRecording) {
				t.Fatalf("accepted malformed recording: %v", err)
			}
		})
	}
}

func TestReplayerVerifyReportsActiveStream(t *testing.T) {
	recording := []byte(`{"version":1,"exchanges":[{"method":"chat_stream","request":` + validWireRequest + `,"stream":{"chunks":null,"outer_error":null,"stream_error":null,"completion":"complete"}}]}`)
	replay, err := NewReplayer(recording)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := replay.ChatStream(context.Background(), []llm.Message{}, nil); err != nil {
		t.Fatal(err)
	}
	verification := new(ScriptVerificationError)
	if err := replay.Verify(); !errors.As(err, &verification) || !verification.Active {
		t.Fatalf("Verify = %#v", err)
	}
}

func streamWire(chunks, fields string) string {
	return `{"version":1,"exchanges":[{"method":"chat_stream","request":` + validWireRequest + `,"stream":{"chunks":` + chunks + `,` + fields + `}}]}`
}
