package openai

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dailz1/go-agent/llm"
)

func TestContextOverflowIncompleteHTTPBodyIsTransportFailure(t *testing.T) {
	const body = `{"error":{"code":"context_length_exceeded"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(body)+10))
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, body)
	}))
	defer srv.Close()
	p := NewProvider("key", "model", WithBaseURL(srv.URL))
	_, err := p.ChatStream(t.Context(), nil, nil)
	var api *llm.APIError
	if !errors.Is(err, io.ErrUnexpectedEOF) || !errors.As(err, &api) || api.Body != body {
		t.Fatalf("transport and HTTP causes not preserved: %v", err)
	}
	if errors.Is(err, llm.ErrContextOverflow) {
		t.Fatalf("incomplete body classified as overflow: %v", err)
	}
}

func TestContextOverflowHTTPFixtures(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"precise code", 400, `{"error":{"code":"context_length_exceeded"}}`, true},
		{"missing code", 400, `{"error":{"type":"context_length_exceeded","message":"context_length_exceeded"}}`, false},
		{"null code", 400, `{"error":{"code":null,"type":"context_length_exceeded"}}`, false},
		{"empty code", 400, `{"error":{"code":"","type":"context_length_exceeded"}}`, false},
		{"output limit", 400, `{"error":{"code":"max_tokens_exceeded","type":"context_length_exceeded","message":"context_length_exceeded"}}`, false},
		{"output parameter", 400, `{"error":{"code":"max_output_tokens","message":"context_length_exceeded"}}`, false},
		{"invalid parameter", 400, `{"error":{"message":"max_tokens must be less than 100"}}`, false},
		{"rate limit", 429, `{"error":{"code":"rate_limit_exceeded","message":"too many tokens"}}`, false},
		{"quota", 429, `{"error":{"code":"insufficient_quota"}}`, false},
		{"usage limit", 429, `{"error":{"code":"usage_limit_reached"}}`, false},
		{"server error", 500, `{"error":{"code":"context_length_exceeded"}}`, false},
		{"unauthorized", 401, `{"error":{"code":"context_length_exceeded"}}`, false},
		{"forbidden", 403, `{"error":{"code":"context_length_exceeded"}}`, false},
		{"large empty", 413, "", false},
		{"large html", 413, "<html>context_length_exceeded</html>", false},
		{"invalid request", 400, `{"error":{"type":"invalid_request"}}`, false},
		{"malformed", 400, `{"error":{"code":"context_length_exceeded"}`, false},
		{"substring code", 400, `{"error":{"code":"prefix_context_length_exceeded"}}`, false},
		{"wrong channel", 400, `{"code":"context_length_exceeded","response":{"error":{"code":"context_length_exceeded"}}}`, false},
	}
	for _, tt := range tests {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tt.name, stream), func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(tt.status)
					fmt.Fprint(w, tt.body)
				}))
				defer srv.Close()
				p := NewProvider("key", "model", WithBaseURL(srv.URL))
				var err error
				if stream {
					_, err = p.ChatStream(t.Context(), nil, nil)
				} else {
					_, _, err = p.Chat(t.Context(), nil, nil)
				}
				if errors.Is(err, llm.ErrContextOverflow) != tt.want {
					t.Fatalf("overflow=%t, want %t: %v", errors.Is(err, llm.ErrContextOverflow), tt.want, err)
				}
				var api *llm.APIError
				if !errors.As(err, &api) || api.StatusCode != tt.status || api.Body != tt.body {
					t.Fatalf("HTTP cause changed: %v", err)
				}
				if tt.want {
					var marker *llm.ContextOverflowError
					if !errors.As(err, &marker) || marker.Code != "context_length_exceeded" || api.Retryable() {
						t.Fatalf("invalid marker: %v", err)
					}
				}
			})
		}
	}
}

func TestContextOverflowLengthIsNotOverflow(t *testing.T) {
	for _, text := range []string{"", "partial"} {
		t.Run(text, func(t *testing.T) {
			payload := fmt.Sprintf(`{"choices":[{"delta":{"content":%q},"finish_reason":"length"}]}`, text)
			chunks, err := parseStreamPayload(payload)
			if err != nil || len(chunks) == 0 || errors.Is(err, llm.ErrContextOverflow) {
				t.Fatalf("length changed: chunks=%v err=%v", chunks, err)
			}
			done, ok := chunks[len(chunks)-1].(llm.DoneChunk)
			if !ok || done.FinishReason != "length" {
				t.Fatalf("length completion lost: %v", chunks)
			}
		})
	}
}
