package glm

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

func serveGLM(t *testing.T, stream bool, bodyCh chan<- []byte) *httptest.Server {
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

func callGLM(t *testing.T, p *Provider, stream bool, opts ...llm.Option) {
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

func TestGLM_RequestFields(t *testing.T) {
	type wireBody struct {
		TopP       *float64 `json:"top_p"`
		ToolChoice *string  `json:"tool_choice"`
		// Parameters with no GLM wire counterpart must stay absent.
		Seed              *int64   `json:"seed"`
		MaxCompletionTok  *int64   `json:"max_completion_tokens"`
		PresencePenalty   *float64 `json:"presence_penalty"`
		FrequencyPenalty  *float64 `json:"frequency_penalty"`
		ParallelToolCalls *bool    `json:"parallel_tool_calls"`
	}

	tests := []struct {
		name      string
		ctorOpts  []ProviderOption
		opts      []llm.Option
		wantTopP  *float64
		wantChoic *string
	}{
		{
			name: "no options omits both",
		},
		{
			name:     "constructor-level top_p applies",
			ctorOpts: []ProviderOption{WithTopP(0.7)},
			wantTopP: f64(0.7),
		},
		{
			name:     "call-level top_p overrides constructor",
			ctorOpts: []ProviderOption{WithTopP(0.7)},
			opts:     []llm.Option{llm.WithTopP(0.9)},
			wantTopP: f64(0.9),
		},
		{
			name:     "call-level top_p alone",
			opts:     []llm.Option{llm.WithTopP(0.9)},
			wantTopP: f64(0.9),
		},
		{
			name: "glm drops all inert fields (frozen contract: never sent)",
			opts: []llm.Option{
				llm.WithSeed(42), llm.WithMaxCompletionTokens(8192),
				llm.WithPresencePenalty(0.5), llm.WithFrequencyPenalty(-0.25),
				llm.WithParallelToolCalls(false),
			},
		},
		{
			name:      "tool_choice auto is sent",
			opts:      []llm.Option{llm.WithToolChoiceAuto()},
			wantChoic: str("auto"),
		},
		{
			name: "tool_choice required is dropped (glm supports auto only)",
			opts: []llm.Option{llm.WithToolChoiceRequired()},
		},
		{
			name: "tool_choice none is dropped",
			opts: []llm.Option{llm.WithToolChoiceNone()},
		},
		{
			name: "tool_choice named tool is dropped",
			opts: []llm.Option{llm.WithToolChoiceTool("lookup")},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, stream := range []bool{false, true} {
				bodyCh := make(chan []byte, 1)
				srv := serveGLM(t, stream, bodyCh)
				provider := NewProvider("key", "model", append(tt.ctorOpts, WithBaseURL(srv.URL))...)
				callGLM(t, provider, stream, tt.opts...)
				srv.Close()

				var body wireBody
				if err := json.Unmarshal(<-bodyCh, &body); err != nil {
					t.Fatalf("decode request body: %v", err)
				}
				want := wireBody{TopP: tt.wantTopP, ToolChoice: tt.wantChoic}
				if !reflect.DeepEqual(body, want) {
					got, _ := json.Marshal(body)
					wantJSON, _ := json.Marshal(want)
					t.Fatalf("wire body = %s, want %s", got, wantJSON)
				}
			}
		})
	}
}

func TestGLM_CachedTokensMapped(t *testing.T) {
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

func TestGLM_StreamChunkUsageHasNoDetails(t *testing.T) {
	// GLM streaming chunk usage carries no prompt_tokens_details; the field
	// stays zero rather than leaking a stale value.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":5}}\n\n")
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
	if done == nil || done.Usage == nil || done.Usage.CachedTokens != 0 {
		t.Fatalf("stream DoneChunk usage = %+v, want zero CachedTokens", done)
	}
}

func f64(v float64) *float64 { return &v }
func str(v string) *string   { return &v }
