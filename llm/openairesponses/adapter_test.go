package openairesponses

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

// TestChatAssemblesOutputItems pins the non-streaming round trip: request
// carries store:false + encrypted-reasoning include + instructions; output
// items assemble into reasoning/text/tool-use blocks in order.
func TestChatAssemblesOutputItems(t *testing.T) {
	var gotBody createRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     "resp_1",
			"status": "completed",
			"output": []map[string]any{
				{"type": "reasoning", "id": "rs_1", "encrypted_content": "enc-abc",
					"summary": []map[string]any{{"type": "summary_text", "text": "thinking"}}},
				{"type": "message", "role": "assistant",
					"content": []map[string]any{{"type": "output_text", "text": "hello"}}},
				{"type": "function_call", "call_id": "call_1", "name": "echo",
					"arguments": `{"x":1}`},
			},
			"usage": map[string]any{"input_tokens": 10, "output_tokens": 5, "total_tokens": 15,
				"output_tokens_details": map[string]any{"reasoning_tokens": 3}},
		})
	}))
	t.Cleanup(srv.Close)
	p := NewProvider("k", "gpt-test", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))

	messages := []llm.Message{
		llm.SystemMessage("be terse"),
		llm.UserMessage("hi"),
	}
	msg, usage, err := p.Chat(context.Background(), messages, nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	if gotBody.Store {
		t.Error("request store = true, want false")
	}
	if len(gotBody.Include) != 1 || gotBody.Include[0] != "reasoning.encrypted_content" {
		t.Errorf("include = %v, want [reasoning.encrypted_content]", gotBody.Include)
	}
	if gotBody.Instructions != "be terse" {
		t.Errorf("instructions = %q", gotBody.Instructions)
	}
	if gotBody.Model != "gpt-test" {
		t.Errorf("model = %q", gotBody.Model)
	}

	if msg.Role != llm.RoleAssistant || len(msg.Content) != 3 {
		t.Fatalf("message = %+v, want 3 blocks", msg)
	}
	ri, ok := msg.Content[0].(llm.ReasoningItemBlock)
	if !ok || ri.ID != "rs_1" || ri.EncryptedContent != "enc-abc" || len(ri.Summary) != 1 {
		t.Errorf("content[0] = %+v, want reasoning item rs_1", msg.Content[0])
	}
	tb, ok := msg.Content[1].(llm.TextBlock)
	if !ok || tb.Text != "hello" {
		t.Errorf("content[1] = %+v, want text hello", msg.Content[1])
	}
	ub, ok := msg.Content[2].(llm.ToolUseBlock)
	if !ok || ub.ID != "call_1" || ub.Name != "echo" {
		t.Errorf("content[2] = %+v, want tool_use call_1", msg.Content[2])
	}
	if usage.InputTokens != 10 || usage.OutputTokens != 5 || usage.ReasoningTokens != 3 {
		t.Errorf("usage = %+v", usage)
	}
}

// TestChatStreamTranslatesEvents pins the event→chunk translation: text and
// reasoning summary deltas, function-call start/args pairing by output index,
// the reasoning item delivered at its own position, usage on the terminal
// chunk, and unknown events ignored without error.
func TestChatStreamTranslatesEvents(t *testing.T) {
	events := []string{
		`{"type":"response.created","response":{"id":"resp_1"}}`,
		`{"type":"response.reasoning_summary_text.delta","output_index":0,"delta":"think"}`,
		`{"type":"response.output_text.delta","output_index":1,"delta":"Hel"}`,
		`{"type":"response.output_text.delta","output_index":1,"delta":"lo"}`,
		`{"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","call_id":"call_9","name":"echo"}}`,
		`{"type":"response.function_call_arguments.delta","output_index":2,"delta":"{\"x\":"}`,
		`{"type":"response.function_call_arguments.delta","output_index":2,"delta":"1}"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1","encrypted_content":"enc"}}`,
		`{"type":"some.future.event","data":"ignored"}`,
		`{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"type":"reasoning","id":"rs_1","encrypted_content":"enc"},{"type":"message","role":"assistant","content":[]},{"type":"function_call","call_id":"call_9","name":"echo","arguments":"{\"x\":1}"}],"usage":{"input_tokens":7,"output_tokens":4,"total_tokens":11}}}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range events {
			_, _ = w.Write([]byte("data: " + ev + "\n\n"))
		}
	}))
	t.Cleanup(srv.Close)
	p := NewProvider("k", "gpt-test", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))

	seq, err := p.ChatStream(context.Background(), []llm.Message{llm.UserMessage("hi")}, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}

	var chunks []llm.Chunk
	for c, err := range seq {
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		chunks = append(chunks, c)
	}

	var texts, reasonings string
	var start *llm.ToolCallStartChunk
	var args string
	var done *llm.DoneChunk
	var itemChunks []llm.ReasoningItemChunk
	var textIndex int64 = -1
	for _, c := range chunks {
		switch e := c.(type) {
		case llm.TextDeltaChunk:
			texts += e.Text
			textIndex = e.OutputIndex
		case llm.ReasoningDeltaChunk:
			reasonings += e.Text
		case llm.ToolCallStartChunk:
			cp := e
			start = &cp
		case llm.ToolCallArgsChunk:
			args += e.Delta
		case llm.ReasoningItemChunk:
			itemChunks = append(itemChunks, e)
		case llm.DoneChunk:
			cp := e
			done = &cp
		}
	}
	if texts != "Hello" {
		t.Errorf("text = %q, want Hello", texts)
	}
	if textIndex != 1 {
		t.Errorf("text output_index = %d, want 1", textIndex)
	}
	if reasonings != "think" {
		t.Errorf("reasoning = %q, want think", reasonings)
	}
	if start == nil || start.ID != "call_9" || start.Index != 2 || start.Name != "echo" {
		t.Errorf("start = %+v, want call_9/echo at index 2", start)
	}
	if args != `{"x":1}` {
		t.Errorf("args = %q", args)
	}
	if done == nil {
		t.Fatal("no DoneChunk")
	}
	if done.Usage == nil || done.Usage.InputTokens != 7 || done.Usage.OutputTokens != 4 {
		t.Errorf("done usage = %+v", done.Usage)
	}
	// The encrypted reasoning item is delivered exactly once: the fixture
	// sends it BOTH via output_item.done and inside the completed payload —
	// the completed handler must dedupe by item ID, not duplicate it.
	if len(itemChunks) != 1 {
		t.Fatalf("reasoning item chunks = %d, want exactly 1 (deduplicated)", len(itemChunks))
	}
	if itemChunks[0].Item.ID != "rs_1" || itemChunks[0].Item.EncryptedContent != "enc" || itemChunks[0].OutputIndex != 0 {
		t.Errorf("reasoning item chunk = %+v, want rs_1 with encrypted content at index 0", itemChunks[0])
	}
}

// TestChatStreamFailedResponse pins that failure and error events surface as
// stream errors.
func TestChatStreamFailedResponse(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event string
	}{
		{"failed", `{"type":"response.failed","response":{"error":{"message":"boom"}}}`},
		{"error", `{"type":"error","message":"boom"}`},
		{"incomplete", `{"type":"response.incomplete","response":{"incomplete_details":{"reason":"max_output_tokens"}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte("data: " + tc.event + "\n\n"))
			}))
			t.Cleanup(srv.Close)
			p := NewProvider("k", "m", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
			seq, err := p.ChatStream(context.Background(), []llm.Message{llm.UserMessage("hi")}, nil)
			if err != nil {
				t.Fatalf("ChatStream: %v", err)
			}
			for _, err := range seq {
				if err == nil {
					t.Error("expected an error yield, got chunks without error")
					break
				}
				return
			}
			t.Error("stream ended without an error yield")
		})
	}
}

// TestConvertMessagesItemSequence pins the message→items mapping: system →
// instructions, user items, assistant fan-out (reasoning before its
// function_call), and tool results as function_call_output.
func TestConvertMessagesItemSequence(t *testing.T) {
	assistant := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		llm.ReasoningItemBlock{Type: "reasoning_item", ID: "rs_1", EncryptedContent: "enc",
			Summary: []string{"thought"}},
		llm.TextBlock{Type: "text", Text: "let me check"},
		llm.ToolUseBlock{Type: "tool_use", ID: "call_1", Name: "echo", Input: json.RawMessage(`{}`)},
	}}
	messages := []llm.Message{
		llm.SystemMessage("sys prompt"),
		llm.UserMessage("go"),
		assistant,
		llm.ToolResultMessage("call_1", tool.NewTextResult("echoed")),
	}

	items, instructions, err := convertMessages(messages)
	if err != nil {
		t.Fatalf("convertMessages: %v", err)
	}
	if instructions != "sys prompt" {
		t.Errorf("instructions = %q", instructions)
	}
	wantTypes := []string{"message", "reasoning", "message", "function_call", "function_call_output"}
	if len(items) != len(wantTypes) {
		t.Fatalf("items = %d, want %d", len(items), len(wantTypes))
	}
	for i, wt := range wantTypes {
		if items[i].Type != wt {
			t.Errorf("items[%d].Type = %q, want %q", i, items[i].Type, wt)
		}
	}
	// The reasoning item precedes its function_call and carries the
	// encrypted content verbatim.
	if items[1].ID != "rs_1" || items[1].EncryptedContent != "enc" {
		t.Errorf("reasoning item = %+v", items[1])
	}
	if items[3].CallID != "call_1" || items[3].Arguments != `{}` {
		t.Errorf("function_call = %+v", items[3])
	}
	if items[4].CallID != "call_1" || items[4].Output == nil || items[4].Output.text != "echoed" {
		t.Errorf("function_call_output = %+v", items[4])
	}
}

// TestConvertToolDefs pins strict:false and schema passthrough.
func TestConvertToolDefs(t *testing.T) {
	defs, err := convertToolDefs([]tool.ToolInfo{{
		Name:        "echo",
		Description: "echoes",
		Parameters: tool.ParameterSchema{Type: "object", Properties: map[string]tool.Property{
			"x": tool.Param("number", "x value"),
		}, Required: []string{"x"}},
	}})
	if err != nil {
		t.Fatalf("convertToolDefs: %v", err)
	}
	if len(defs) != 1 {
		t.Fatalf("defs = %d, want 1", len(defs))
	}
	if defs[0].Type != "function" || defs[0].Name != "echo" || defs[0].Strict {
		t.Errorf("def = %+v, want function/echo/strict=false", defs[0])
	}
	if len(defs[0].Parameters) == 0 || !strings.Contains(string(defs[0].Parameters), `"type":"object"`) {
		t.Errorf("parameters = %s, want the object schema", defs[0].Parameters)
	}
}

func TestConvertToolDefs_ExtendedSchemaWire(t *testing.T) {
	falseValue, trueValue := false, true
	defs, err := convertToolDefs([]tool.ToolInfo{{
		Name:        "extended",
		Description: "extended schema",
		Parameters: tool.ParameterSchema{
			Type: "object",
			Properties: map[string]tool.Property{
				"choice":        {AnyOf: []*tool.Property{{Type: "string", Description: "choice"}, {BooleanSchema: &falseValue}}},
				"config":        {Type: "object", Properties: map[string]tool.Property{"enabled": {Type: "boolean", Description: "enabled"}}, Required: []string{"enabled"}, AdditionalProperties: &falseValue},
				"labels":        {Type: "array", Nullable: true, Items: &tool.Property{Type: "string", Description: "label"}},
				"metadata":      {Type: "object", AdditionalPropertiesSchema: &tool.Property{Type: "string", Description: "metadata value"}},
				"reference":     {Ref: "#/$defs/value"},
				"unconstrained": {BooleanSchema: &trueValue},
			},
			Required:             []string{"choice", "config", "labels", "metadata", "reference", "unconstrained"},
			AdditionalProperties: &falseValue,
			Keywords:             []tool.SchemaKeyword{{Name: "x-vendor", Value: json.RawMessage(`{"enabled":true}`)}, {Name: "$defs", Value: json.RawMessage(`{"value":{"type":"integer"}}`)}},
		},
	}})
	if err != nil {
		t.Fatalf("convertToolDefs: %v", err)
	}
	got, err := json.Marshal(defs)
	if err != nil {
		t.Fatalf("marshal definitions: %v", err)
	}
	const want = `[{"type":"function","name":"extended","description":"extended schema","parameters":{"type":"object","properties":{"choice":{"anyOf":[{"type":"string","description":"choice"},false]},"config":{"type":"object","properties":{"enabled":{"type":"boolean","description":"enabled"}},"required":["enabled"],"additionalProperties":false},"labels":{"type":["null","array"],"items":{"type":"string","description":"label"}},"metadata":{"type":"object","additionalProperties":{"type":"string","description":"metadata value"}},"reference":{"$ref":"#/$defs/value"},"unconstrained":true},"required":["choice","config","labels","metadata","reference","unconstrained"],"additionalProperties":false,"$defs":{"value":{"type":"integer"}},"x-vendor":{"enabled":true}},"strict":false}]`
	if string(got) != want {
		t.Fatalf("wire JSON = %s\nwant %s", got, want)
	}
}

// --- eager setup (R series) ---

const completedEvent = `{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`

// TestChatStreamSetupIsEager pins the eager-setup contract: the HTTP request
// has been issued by the time ChatStream returns, before any ranging begins.
func TestChatStreamSetupIsEager(t *testing.T) {
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: " + completedEvent + "\n\n"))
	}))
	t.Cleanup(srv.Close)
	p := NewProvider("k", "gpt-test", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	seq, err := p.ChatStream(ctx, []llm.Message{llm.UserMessage("hi")}, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if seq == nil {
		t.Fatal("ChatStream seq = nil")
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests before ranging = %d, want 1 (setup must be eager)", got)
	}
	cancel()
	for _, err := range seq {
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("unexpected stream error: %v", err)
		}
	}
}

// TestChatStreamSetupErrorIsSync pins that non-2xx setup failures surface as
// a synchronous (nil, err) return carrying *llm.APIError — the channel the
// agent's pre-stream retry loop consumes.
func TestChatStreamSetupErrorIsSync(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)
	p := NewProvider("k", "gpt-test", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))

	seq, err := p.ChatStream(context.Background(), []llm.Message{llm.UserMessage("hi")}, nil)
	if err == nil {
		t.Fatal("ChatStream err = nil, want synchronous setup error")
	}
	if seq != nil {
		t.Fatal("ChatStream seq != nil on setup error")
	}
	var apiErr *llm.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadGateway {
		t.Fatalf("err = %v, want *llm.APIError with status 502", err)
	}
}

// TestChatStreamSetupErrorRetryable pins that a 503 setup failure classifies
// as retryable and the second call succeeds with no residual state.
func TestChatStreamSetupErrorRetryable(t *testing.T) {
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: " + completedEvent + "\n\n"))
	}))
	t.Cleanup(srv.Close)
	p := NewProvider("k", "gpt-test", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))

	_, err := p.ChatStream(context.Background(), []llm.Message{llm.UserMessage("hi")}, nil)
	if err == nil {
		t.Fatal("first ChatStream err = nil, want 503 setup error")
	}
	if !llm.IsRetryableError(err) {
		t.Fatalf("err = %v, want retryable", err)
	}

	seq, err := p.ChatStream(context.Background(), []llm.Message{llm.UserMessage("hi")}, nil)
	if err != nil {
		t.Fatalf("second ChatStream: %v", err)
	}
	sawDone := false
	for c, err := range seq {
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		if _, ok := c.(llm.DoneChunk); ok {
			sawDone = true
		}
	}
	if !sawDone {
		t.Fatal("stream ended without DoneChunk")
	}
}

// TestParseStreamEventEmptyPayloadSkipped pins the empty-payload guard: a
// bare keepalive frame must not reach the JSON decoder.
func TestParseStreamEventEmptyPayloadSkipped(t *testing.T) {
	p := NewProvider("k", "gpt-test")
	for _, payload := range []string{"", "   ", "\t\n"} {
		chunks, err := p.parseStreamEvent(payload, map[string]bool{})
		if err != nil || chunks != nil {
			t.Errorf("parseStreamEvent(%q) = (%v, %v), want (nil, nil)", payload, chunks, err)
		}
	}
}

// TestChatStreamIgnoresEmptyDataFrames pins that interleaved empty data
// frames and comment frames no longer kill the stream.
func TestChatStreamIgnoresEmptyDataFrames(t *testing.T) {
	var body strings.Builder
	body.WriteString("data: \n\n")
	body.WriteString(": ping\n\n")
	body.WriteString("data:   \n\n")
	body.WriteString("data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"hi\"}\n\n")
	body.WriteString("data: \n\n")
	body.WriteString("data: " + completedEvent + "\n\n")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(body.String()))
	}))
	t.Cleanup(srv.Close)
	p := NewProvider("k", "gpt-test", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))

	seq, err := p.ChatStream(context.Background(), []llm.Message{llm.UserMessage("hi")}, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	sawDone := false
	for c, err := range seq {
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		if _, ok := c.(llm.DoneChunk); ok {
			sawDone = true
		}
	}
	if !sawDone {
		t.Fatal("stream ended without DoneChunk")
	}
}

// TestToolOutputItemImageParts pins the three wire byte-rules of the
// function_call_output union: text-only marshals as a JSON string identical
// to the pre-union wire; empty content without images omits the field
// entirely; images emit an ordered input_text + input_image array.
func TestToolOutputItemImageParts(t *testing.T) {
	messages := []llm.Message{
		{Role: llm.RoleTool, Content: []llm.ContentBlock{
			llm.ToolResultBlock{Type: "tool_result", ToolUseID: "call_1", Content: "echoed"}},
		},
		{Role: llm.RoleTool, Content: []llm.ContentBlock{
			llm.ToolResultBlock{Type: "tool_result", ToolUseID: "call_2"}},
		},
		{Role: llm.RoleTool, Content: []llm.ContentBlock{
			llm.ToolResultBlock{Type: "tool_result", ToolUseID: "call_3", Content: "shot",
				Images: []llm.ImageBlock{
					{Type: "image", URL: "https://example.com/a.png"},
					{Type: "image", MIMEType: "image/png", Data: []byte("png")},
				},
			},
		},
		},
	}
	items, _, err := convertMessages(messages)
	if err != nil {
		t.Fatalf("convertMessages: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("items = %d, want 3", len(items))
	}
	// Rule 1: text-only → JSON string, wire-identical to the pre-union field.
	textOnly, err := json.Marshal(items[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(textOnly), `"output":"echoed"`) {
		t.Errorf("text-only wire = %s, want plain string output", textOnly)
	}
	// Rule 2: empty content without images → field omitted.
	empty, err := json.Marshal(items[1])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(empty), `"output"`) {
		t.Errorf("empty wire = %s, want omitted output field", empty)
	}
	// Rule 3: images → ordered typed-parts array with a data URL for bytes.
	out := items[2].Output
	if out == nil || len(out.parts) != 3 {
		t.Fatalf("output = %+v, want 3 parts", items[2].Output)
	}
	if out.parts[0].Type != "input_text" || out.parts[0].Text != "shot" {
		t.Errorf("parts[0] = %+v, want input_text shot", out.parts[0])
	}
	if out.parts[1].Type != "input_image" || out.parts[1].ImageURL != "https://example.com/a.png" {
		t.Errorf("parts[1] = %+v, want URL form", out.parts[1])
	}
	if out.parts[2].Type != "input_image" || out.parts[2].ImageURL != "data:image/png;base64,cG5n" {
		t.Errorf("parts[2] = %+v, want inline data-URL form", out.parts[2])
	}
}
