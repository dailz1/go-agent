package codex

import (
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/dailz1/go-agent/llm"
)

func TestContextOverflowInterruptedSSEFrameIsTransportFailure(t *testing.T) {
	body := io.MultiReader(strings.NewReader("data: {\"type\":\"error\",\"code\":\"context_length_exceeded\"}\n"),
		iotest.ErrReader(io.ErrUnexpectedEOF))
	var err error
	for _, streamErr := range scanResponse(t.Context(), body) {
		err = streamErr
		break
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, llm.ErrContextOverflow) {
		t.Fatalf("interrupted frame classified: %v", err)
	}
}

func TestStreamErrorLegacyQuotaLookupAndResponsePrecedence(t *testing.T) {
	for _, payload := range []string{
		`{"type":"response.failed","error":{"code":"usage_limit_reached","resets_at":"2030-01-01T00:00:00Z"}}`,
		`{"type":"error","error":{"code":"other"},"response":{"error":{"code":"usage_limit_reached","resets_at":"2030-01-01T00:00:00Z"}}}`,
		`{"type":"error","code":"context_length_exceeded","error":{"code":"usage_limit_reached","resets_at":"2030-01-01T00:00:00Z"}}`,
		`{"type":"error","error":{"code":"context_length_exceeded"},"response":{"error":{"code":"usage_limit_reached","resets_at":"2030-01-01T00:00:00Z"}}}`,
	} {
		t.Run(payload, func(t *testing.T) {
			var err error
			for _, streamErr := range scanResponse(t.Context(), strings.NewReader(sse(payload))) {
				err = streamErr
				break
			}
			var original *Error
			wantReset := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
			if !errors.As(err, &original) || original.Kind != KindQuota ||
				original.Code != "usage_limit_reached" || original.RetryAt == nil || !original.RetryAt.Equal(wantReset) {
				t.Fatalf("legacy quota diagnostic lost: %v", err)
			}
			if errors.Is(err, llm.ErrContextOverflow) {
				t.Fatalf("quota classified as overflow: %v", err)
			}
		})
	}
}

func TestContextOverflowStreamFixtures(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    bool
		code    string
		kind    string
	}{
		{"failed", `{"type":"response.failed","response":{"error":{"code":"context_length_exceeded"}}}`, true, "context_length_exceeded", KindProtocol},
		{"top error", `{"type":"error","code":"context_length_exceeded"}`, true, "context_length_exceeded", KindProtocol},
		{"top with missing nested code", `{"type":"error","code":"context_length_exceeded","error":{}}`, true, "", KindProtocol},
		{"top with empty nested code", `{"type":"error","code":"context_length_exceeded","error":{"code":""}}`, true, "", KindProtocol},
		{"top with null nested code", `{"type":"error","code":"context_length_exceeded","error":{"code":null}}`, true, "", KindProtocol},
		{"top other defeats nested overflow", `{"type":"error","code":"other","error":{"code":"context_length_exceeded"}}`, false, "context_length_exceeded", KindProtocol},
		{"nested error", `{"type":"error","error":{"code":"context_length_exceeded"}}`, true, "context_length_exceeded", KindProtocol},
		{"failed missing code", `{"type":"response.failed","response":{"error":{"type":"context_length_exceeded"}}}`, false, "context_length_exceeded", KindProtocol},
		{"failed null code", `{"type":"response.failed","response":{"error":{"code":null,"type":"context_length_exceeded"}}}`, false, "context_length_exceeded", KindProtocol},
		{"failed empty code", `{"type":"response.failed","response":{"error":{"code":"","type":"context_length_exceeded"}}}`, false, "context_length_exceeded", KindProtocol},
		{"error missing code", `{"type":"error","error":{"type":"context_length_exceeded"}}`, false, "context_length_exceeded", KindProtocol},
		{"error null code", `{"type":"error","code":null,"message":"context_length_exceeded"}`, false, "", KindProtocol},
		{"failed output limit", `{"type":"response.failed","response":{"error":{"code":"max_tokens_exceeded","type":"context_length_exceeded","message":"context_length_exceeded"}}}`, false, "max_tokens_exceeded", KindProtocol},
		{"error output parameter", `{"type":"error","error":{"code":"max_output_tokens","type":"context_length_exceeded","message":"context_length_exceeded"}}`, false, "max_output_tokens", KindProtocol},
		{"failed top misplaced", `{"type":"response.failed","code":"context_length_exceeded"}`, false, "context_length_exceeded", KindProtocol},
		{"failed error misplaced", `{"type":"response.failed","error":{"code":"context_length_exceeded"}}`, false, "context_length_exceeded", KindProtocol},
		{"error response misplaced", `{"type":"error","response":{"error":{"code":"context_length_exceeded"}}}`, false, "context_length_exceeded", KindProtocol},
		{"incomplete tokens", `{"type":"response.incomplete","response":{"incomplete_details":{"reason":"max_output_tokens"}}}`, false, "", KindProtocol},
		{"incomplete filter", `{"type":"response.incomplete","response":{"incomplete_details":{"reason":"content_filter"}}}`, false, "", KindProtocol},
		{"incomplete distractors", `{"type":"response.incomplete","code":"context_length_exceeded","error":{"code":"context_length_exceeded"},"response":{"incomplete_details":{"reason":"max_output_tokens"},"error":{"code":"context_length_exceeded","type":"context_length_exceeded","message":"context_length_exceeded"}}}`, false, "context_length_exceeded", KindProtocol},
		{"quota", `{"type":"response.failed","response":{"error":{"code":"insufficient_quota"}}}`, false, "insufficient_quota", KindQuota},
		{"usage limit", `{"type":"error","error":{"code":"usage_limit_reached"}}`, false, "usage_limit_reached", KindQuota},
		{"malformed", `{"type":"response.failed","response":{"error":{"code":"context_length_exceeded"}`, false, "", KindProtocol},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var err error
			for _, streamErr := range scanResponse(t.Context(), strings.NewReader(sse(tt.payload))) {
				if streamErr != nil {
					err = streamErr
					break
				}
			}
			if err == nil || errors.Is(err, llm.ErrContextOverflow) != tt.want {
				t.Fatalf("overflow want %t: %v", tt.want, err)
			}
			var original *Error
			if !errors.As(err, &original) || original.Kind != tt.kind || original.Code != tt.code {
				t.Fatalf("Codex cause changed: %v", err)
			}
			var api *llm.APIError
			if errors.As(err, &api) {
				t.Fatalf("SSE fabricated an APIError: %v", err)
			}
			if tt.want {
				var marker *llm.ContextOverflowError
				if !errors.As(err, &marker) || marker.Code != "context_length_exceeded" || marker.Err != original {
					t.Fatalf("marker lost original Codex error: %v", err)
				}
			}
		})
	}
}

func TestContextOverflowRequiresDispatchedSSEFrame(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       bool
	}{
		{"complete", "data: {\"type\":\"error\",\"code\":\"context_length_exceeded\"}\n\n", true},
		{"clean eof", "data: {\"type\":\"error\",\"code\":\"context_length_exceeded\"}\n", false},
		{"split string", "data: {\"type\":\"error\",\"code\":\"context_length_\ndata: exceeded\"}\n\n", false},
		{"valid multiline", "data: {\"type\":\"error\",\ndata: \"code\":\"context_length_exceeded\"}\n\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got error
			for _, err := range scanResponse(t.Context(), strings.NewReader(tc.body)) {
				if err != nil {
					got = err
					break
				}
			}
			if got == nil || errors.Is(got, llm.ErrContextOverflow) != tc.want {
				t.Fatalf("overflow=%t, want %t: %v", errors.Is(got, llm.ErrContextOverflow), tc.want, got)
			}
		})
	}
}
