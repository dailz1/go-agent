package glm

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

func TestProvider_CallLevelThinkingPriority(t *testing.T) {
	tests := []struct {
		name       string
		consOpts   []ProviderOption
		callOpts   []llm.Option
		wantType   string
		wantClear  bool
		wantAbsent bool
	}{
		{
			name:      "constructor only keeps clear_thinking",
			consOpts:  []ProviderOption{WithThinkingEnabled(), WithClearThinking(true)},
			wantType:  "enabled",
			wantClear: true,
		},
		{
			name:     "call level overrides constructor",
			consOpts: []ProviderOption{WithThinkingEnabled(), WithClearThinking(true)},
			callOpts: []llm.Option{llm.WithThinking("disabled")},
			wantType: "disabled",
		},
		{
			name:       "no thinking anywhere omits field",
			wantAbsent: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bodyCh := make(chan []byte, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				bodyCh <- body
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
			}))
			defer srv.Close()

			opts := append([]ProviderOption{WithBaseURL(srv.URL)}, tt.consOpts...)
			provider := NewProvider("key", "glm-4", opts...)

			var callOpts []llm.Option
			callOpts = append(callOpts, tt.callOpts...)

			if _, _, err := provider.Chat(context.Background(), []llm.Message{llm.UserMessage("hi")}, nil, callOpts...); err != nil {
				t.Fatalf("Chat: %v", err)
			}

			var body struct {
				Thinking *struct {
					Type          string `json:"type"`
					ClearThinking bool   `json:"clear_thinking"`
				} `json:"thinking"`
			}
			if err := json.Unmarshal(<-bodyCh, &body); err != nil {
				t.Fatalf("decode request body: %v", err)
			}

			if tt.wantAbsent {
				if body.Thinking != nil {
					t.Fatalf("thinking = %+v, want absent", body.Thinking)
				}
				return
			}
			if body.Thinking == nil || body.Thinking.Type != tt.wantType {
				t.Fatalf("thinking = %+v, want type %q", body.Thinking, tt.wantType)
			}
			if body.Thinking.ClearThinking != tt.wantClear {
				t.Fatalf("clear_thinking = %v, want %v", body.Thinking.ClearThinking, tt.wantClear)
			}
		})
	}
}
