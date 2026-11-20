package codex

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dailz1/go-agent/llm"
)

func TestContextOverflowHTTPFixtures(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   bool
		kind   string
	}{
		{"precise code", 400, `{"error":{"code":"context_length_exceeded"}}`, true, KindCapability},
		{"missing code", 400, `{"error":{"type":"context_length_exceeded","message":"context_length_exceeded"}}`, false, KindCapability},
		{"null code", 400, `{"error":{"code":null,"type":"context_length_exceeded"}}`, false, KindCapability},
		{"empty code", 400, `{"error":{"code":"","type":"context_length_exceeded"}}`, false, KindCapability},
		{"output limit", 400, `{"error":{"code":"max_tokens_exceeded","type":"context_length_exceeded","message":"context_length_exceeded"}}`, false, KindCapability},
		{"output parameter", 400, `{"error":{"code":"max_output_tokens","type":"context_length_exceeded","message":"context_length_exceeded"}}`, false, KindCapability},
		{"invalid parameter", 400, `{"error":{"message":"max_output_tokens must be less than 100"}}`, false, KindCapability},
		{"rate limit", 429, `{"error":{"code":"rate_limit_exceeded","message":"too many tokens"}}`, false, ""},
		{"quota", 429, `{"error":{"code":"insufficient_quota"}}`, false, KindQuota},
		{"usage limit", 429, `{"error":{"code":"usage_limit_reached"}}`, false, KindQuota},
		{"server error", 500, `{"error":{"code":"context_length_exceeded"}}`, false, ""},
		{"unauthorized", 401, `{"error":{"code":"context_length_exceeded"}}`, false, KindAuth},
		{"forbidden", 403, `{"error":{"code":"context_length_exceeded"}}`, false, KindCapability},
		{"large empty", 413, "", false, ""},
		{"large html", 413, "<html>context_length_exceeded</html>", false, ""},
		{"invalid request", 400, `{"error":{"type":"invalid_request"}}`, false, KindCapability},
		{"malformed", 400, `{"error":{"code":"context_length_exceeded"}`, false, KindCapability},
		{"substring code", 400, `{"error":{"code":"prefix_context_length_exceeded"}}`, false, KindCapability},
		{"top code misplaced", 400, `{"code":"context_length_exceeded"}`, false, KindCapability},
		{"response misplaced", 400, `{"response":{"error":{"code":"context_length_exceeded"}}}`, false, KindCapability},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				fmt.Fprint(w, tt.body)
			}))
			defer srv.Close()
			p := NewProvider("model", WithAuthSource(&testSource{}), WithBaseURL(srv.URL))
			_, err := p.ChatStream(t.Context(), nil, nil)
			if errors.Is(err, llm.ErrContextOverflow) != tt.want {
				t.Fatalf("overflow=%t, want %t: %v", errors.Is(err, llm.ErrContextOverflow), tt.want, err)
			}
			var api *llm.APIError
			if !errors.As(err, &api) || api.StatusCode != tt.status || api.Body != tt.body {
				t.Fatalf("HTTP cause changed: %v", err)
			}
			var original *Error
			if tt.kind != "" && (!errors.As(err, &original) || original.Kind != tt.kind) {
				t.Fatalf("Codex kind changed, want %s: %v", tt.kind, err)
			}
			if tt.kind == KindQuota && api.Retryable() {
				t.Fatalf("quota became retryable: %v", err)
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
