package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/dailz1/go-agent/llm"
)

// serveChat captures each request body on bodyCh and answers with a sync
// JSON response, or a minimal SSE stream when stream is true.
func serveChat(t *testing.T, stream bool, bodyCh chan<- []byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		bodyCh <- body

		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
}

func callProvider(t *testing.T, p *Provider, stream bool, opts ...llm.Option) {
	t.Helper()
	if stream {
		seq, err := p.ChatStream(context.Background(), []llm.Message{llm.UserMessage("hi")}, nil, opts...)
		if err != nil {
			t.Fatalf("ChatStream: %v", err)
		}
		for _, err := range seq {
			if err != nil {
				t.Fatalf("consume stream: %v", err)
			}
		}
		return
	}
	if _, _, err := p.Chat(context.Background(), []llm.Message{llm.UserMessage("hi")}, nil, opts...); err != nil {
		t.Fatalf("Chat: %v", err)
	}
}

func TestProvider_SamplingAndToolChoiceRequestFields(t *testing.T) {
	type wireBody struct {
		MaxTokens         *int     `json:"max_tokens"`
		MaxCompletionTok  *int64   `json:"max_completion_tokens"`
		TopP              *float64 `json:"top_p"`
		Seed              *int64   `json:"seed"`
		PresencePenalty   *float64 `json:"presence_penalty"`
		FrequencyPenalty  *float64 `json:"frequency_penalty"`
		ParallelToolCalls *bool    `json:"parallel_tool_calls"`
		ToolChoice        any      `json:"tool_choice"`
	}
	setI64 := func(v int64) *int64 { return &v }
	setF64 := func(v float64) *float64 { return &v }
	setB := func(v bool) *bool { return &v }
	setI := func(v int) *int { return &v }

	tests := []struct {
		name   string
		stream bool
		opts   []llm.Option
		want   wireBody
	}{
		{
			name: "chat without options omits all new fields",
		},
		{
			name:   "stream without options omits all new fields",
			stream: true,
		},
		{
			name: "chat with sampling fields",
			opts: []llm.Option{
				llm.WithTopP(0.9), llm.WithSeed(42),
				llm.WithPresencePenalty(0.5), llm.WithFrequencyPenalty(-0.25),
			},
			want: wireBody{TopP: setF64(0.9), Seed: setI64(42), PresencePenalty: setF64(0.5), FrequencyPenalty: setF64(-0.25)},
		},
		{
			name:   "stream with sampling fields",
			stream: true,
			opts:   []llm.Option{llm.WithTopP(0.9), llm.WithSeed(42)},
			want:   wireBody{TopP: setF64(0.9), Seed: setI64(42)},
		},
		{
			name: "parallel_tool_calls false is present",
			opts: []llm.Option{llm.WithParallelToolCalls(false)},
			want: wireBody{ParallelToolCalls: setB(false)},
		},
		{
			name: "max_completion_tokens suppresses max_tokens",
			opts: []llm.Option{llm.WithMaxTokens(1024), llm.WithMaxCompletionTokens(8192)},
			want: wireBody{MaxCompletionTok: setI64(8192)},
		},
		{
			name: "max_completion_tokens alone",
			opts: []llm.Option{llm.WithMaxCompletionTokens(8192)},
			want: wireBody{MaxCompletionTok: setI64(8192)},
		},
		{
			name: "max_tokens alone stays",
			opts: []llm.Option{llm.WithMaxTokens(1024)},
			want: wireBody{MaxTokens: setI(1024)},
		},
		{
			name: "tool_choice mode string",
			opts: []llm.Option{llm.WithToolChoiceRequired()},
			want: wireBody{ToolChoice: "required"},
		},
		{
			name: "tool_choice none string",
			opts: []llm.Option{llm.WithToolChoiceNone()},
			want: wireBody{ToolChoice: "none"},
		},
		{
			name: "tool_choice auto string",
			opts: []llm.Option{llm.WithToolChoiceAuto()},
			want: wireBody{ToolChoice: "auto"},
		},
		{
			name: "tool_choice named tool object",
			opts: []llm.Option{llm.WithToolChoiceTool("lookup")},
			want: wireBody{ToolChoice: map[string]any{"type": "function", "function": map[string]any{"name": "lookup"}}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bodyCh := make(chan []byte, 1)
			srv := serveChat(t, tt.stream, bodyCh)
			defer srv.Close()

			provider := NewProvider("key", "model", WithBaseURL(srv.URL))
			callProvider(t, provider, tt.stream, tt.opts...)

			var body wireBody
			if err := json.Unmarshal(<-bodyCh, &body); err != nil {
				t.Fatalf("decode request body: %v", err)
			}

			gotJSON, _ := json.Marshal(body)
			wantJSON, _ := json.Marshal(tt.want)
			if !reflect.DeepEqual(body, tt.want) {
				t.Fatalf("wire body = %s, want %s", gotJSON, wantJSON)
			}
		})
	}
}

func TestProvider_ToolChoiceZeroValueOmitted(t *testing.T) {
	bodyCh := make(chan []byte, 1)
	srv := serveChat(t, false, bodyCh)
	defer srv.Close()

	provider := NewProvider("key", "model", WithBaseURL(srv.URL))
	// Zero-value literal is "unset" and must be omitted, not serialized.
	zeroChoice := &llm.ToolChoice{}
	callProvider(t, provider, false, func(o *llm.Options) { o.ToolChoice = zeroChoice })

	var body struct {
		ToolChoice any `json:"tool_choice"`
	}
	if err := json.Unmarshal(<-bodyCh, &body); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	if body.ToolChoice != nil {
		t.Fatalf("tool_choice = %v, want absent for zero value", body.ToolChoice)
	}
}

func TestProvider_CachedTokensMapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":100,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":80}}}`)
	}))
	defer srv.Close()

	provider := NewProvider("key", "model", WithBaseURL(srv.URL))
	_, usage, err := provider.Chat(context.Background(), []llm.Message{llm.UserMessage("hi")}, nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if usage.CachedTokens != 80 {
		t.Fatalf("CachedTokens = %d, want 80", usage.CachedTokens)
	}
}

func TestProvider_CachedTokensStreamFinalBlock(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":5,\"prompt_tokens_details\":{\"cached_tokens\":64}}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	provider := NewProvider("key", "model", WithBaseURL(srv.URL))
	seq, err := provider.ChatStream(context.Background(), []llm.Message{llm.UserMessage("hi")}, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	var done *llm.DoneChunk
	for chunk, err := range seq {
		if err != nil {
			t.Fatalf("consume stream: %v", err)
		}
		if d, ok := chunk.(llm.DoneChunk); ok {
			done = &d
		}
	}
	if done == nil || done.Usage == nil || done.Usage.CachedTokens != 64 {
		t.Fatalf("stream final usage CachedTokens = %+v, want 64", done)
	}
}

func TestProvider_ThinkingAndJSONModeRequestFields(t *testing.T) {
	tests := []struct {
		name       string
		stream     bool
		opts       []llm.Option
		wantThink  string
		wantFormat string
	}{
		{
			name:      "chat with thinking disabled",
			opts:      []llm.Option{llm.WithThinking("disabled")},
			wantThink: "disabled",
		},
		{
			name:      "chat with thinking enabled",
			opts:      []llm.Option{llm.WithThinking("enabled")},
			wantThink: "enabled",
		},
		{
			name:       "chat with json mode",
			opts:       []llm.Option{llm.WithJSONMode()},
			wantFormat: "json_object",
		},
		{
			name:       "chat with both",
			opts:       []llm.Option{llm.WithThinking("disabled"), llm.WithJSONMode()},
			wantThink:  "disabled",
			wantFormat: "json_object",
		},
		{
			name: "chat without options omits both",
		},
		{
			name:      "stream with thinking",
			stream:    true,
			opts:      []llm.Option{llm.WithThinking("disabled")},
			wantThink: "disabled",
		},
		{
			name:       "stream with json mode",
			stream:     true,
			opts:       []llm.Option{llm.WithJSONMode()},
			wantFormat: "json_object",
		},
		{
			name:   "stream without options omits both",
			stream: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bodyCh := make(chan []byte, 1)
			srv := serveChat(t, tt.stream, bodyCh)
			defer srv.Close()

			provider := NewProvider("key", "model", WithBaseURL(srv.URL))
			callProvider(t, provider, tt.stream, tt.opts...)

			var body struct {
				Thinking *struct {
					Type string `json:"type"`
				} `json:"thinking"`
				ResponseFormat *struct {
					Type string `json:"type"`
				} `json:"response_format"`
			}
			if err := json.Unmarshal(<-bodyCh, &body); err != nil {
				t.Fatalf("decode request body: %v", err)
			}

			if tt.wantThink == "" {
				if body.Thinking != nil {
					t.Fatalf("thinking = %+v, want absent", body.Thinking)
				}
			} else if body.Thinking == nil || body.Thinking.Type != tt.wantThink {
				t.Fatalf("thinking = %+v, want type %q", body.Thinking, tt.wantThink)
			}

			if tt.wantFormat == "" {
				if body.ResponseFormat != nil {
					t.Fatalf("response_format = %+v, want absent", body.ResponseFormat)
				}
			} else if body.ResponseFormat == nil || body.ResponseFormat.Type != tt.wantFormat {
				t.Fatalf("response_format = %+v, want type %q", body.ResponseFormat, tt.wantFormat)
			}
		})
	}
}
