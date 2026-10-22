package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
