package openairesponses

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestContextOverflowInterruptedSSEFrameIsTransportFailure(t *testing.T) {
	const body = "data: {\"type\":\"error\",\"code\":\"context_length_exceeded\"}\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(body)+10))
		fmt.Fprint(w, body)
	}))
	defer srv.Close()
	p := NewProvider("key", "model", WithBaseURL(srv.URL))
	seq, err := p.ChatStream(t.Context(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, streamErr := range seq {
		err = streamErr
		break
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, llm.ErrContextOverflow) {
		t.Fatalf("interrupted frame classified: %v", err)
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
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			p := NewProvider("key", "model", WithBaseURL(srv.URL))
			seq, err := p.ChatStream(t.Context(), nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, streamErr := range seq {
				if streamErr != nil {
					err = streamErr
					break
				}
			}
			if err == nil || errors.Is(err, llm.ErrContextOverflow) != tc.want {
				t.Fatalf("overflow=%t, want %t: %v", errors.Is(err, llm.ErrContextOverflow), tc.want, err)
			}
		})
	}
}

func TestResponseFailureOptionalDiagnosticsDoNotVetoCode(t *testing.T) {
	for _, channel := range []string{"http", "chat", "failed", "error"} {
		t.Run(channel, func(t *testing.T) {
			detail := `{"code":"context_length_exceeded","type":123,"param":0,"message":{"detail":"too long"}}`
			body := `{"status":"failed","error":` + detail + `}`
			switch channel {
			case "failed":
				body = "data: {\"type\":\"response.failed\",\"response\":" + body + "}\n\n"
			case "error":
				body = "data: {\"type\":\"error\",\"code\":\"context_length_exceeded\",\"param\":0,\"message\":123}\n\n"
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if channel == "http" {
					w.WriteHeader(http.StatusBadRequest)
				}
				fmt.Fprint(w, body)
			}))
			defer srv.Close()
			p := NewProvider("key", "model", WithBaseURL(srv.URL))
			var err error
			if channel == "error" || channel == "failed" {
				seq, setupErr := p.ChatStream(t.Context(), nil, nil)
				if setupErr != nil {
					t.Fatal(setupErr)
				}
				for _, streamErr := range seq {
					err = streamErr
					break
				}
			} else {
				_, _, err = p.Chat(t.Context(), nil, nil)
			}
			var diagnostic *errorEnvelope
			if !errors.Is(err, llm.ErrContextOverflow) || !errors.As(err, &diagnostic) {
				t.Fatalf("diagnostic masked exact overflow code: %v", err)
			}
			if string(diagnostic.Details["param"]) != "0" || len(diagnostic.Details["message"]) == 0 {
				t.Fatalf("non-string diagnostics discarded: %+v", diagnostic)
			}
		})
	}
}

func TestContextOverflowRejectsOversizedFailedEnvelope(t *testing.T) {
	body := `{"status":"failed","error":{"code":"context_length_exceeded"}}`
	body += strings.Repeat(" ", llm.MaxResponseBody-len(body)) + "x"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, body)
	}))
	defer srv.Close()
	p := NewProvider("key", "model", WithBaseURL(srv.URL))
	_, _, err := p.Chat(t.Context(), nil, nil)
	if err == nil || errors.Is(err, llm.ErrContextOverflow) {
		t.Fatalf("oversized successful response classified from prefix: %T", err)
	}
}

func TestResponseCancelledPreservesProviderError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"cancelled","error":{"code":"context_length_exceeded","message":"provider cancelled"}}`)
	}))
	defer srv.Close()
	p := NewProvider("key", "model", WithBaseURL(srv.URL))
	_, _, err := p.Chat(t.Context(), nil, nil)
	var diagnostic *errorEnvelope
	if errors.Is(err, llm.ErrContextOverflow) || !errors.As(err, &diagnostic) || diagnostic.Message != "provider cancelled" {
		t.Fatalf("cancelled response lost provider error: %v", err)
	}
}

func TestResponseFutureEventIgnoresErrorDiagnosticFields(t *testing.T) {
	p := NewProvider("key", "model")
	chunks, err := p.parseStreamEvent(`{"type":"future.lifecycle","code":123,"param":{"future":true},"message":0}`, map[string]bool{})
	if err != nil || len(chunks) != 0 {
		t.Fatalf("future event decoded unrelated diagnostic fields: %v, %v", chunks, err)
	}
}

func TestResponseFailuresPreserveStructuredDiagnostics(t *testing.T) {
	for _, code := range []string{"max_output_tokens", "context_length_exceeded"} {
		for _, channel := range []string{"http", "chat", "failed", "error"} {
			t.Run(code+"/"+channel, func(t *testing.T) {
				detail := fmt.Sprintf(`{"code":%q,"type":"invalid_request_error","param":"max_output_tokens","message":"rejected"}`, code)
				body := `{"status":"failed","error":` + detail + `}`
				wantType := "invalid_request_error"
				switch channel {
				case "failed":
					body = "data: {\"type\":\"response.failed\",\"response\":" + body + "}\n\n"
				case "error":
					body = fmt.Sprintf("data: {\"type\":\"error\",\"code\":%q,\"param\":\"max_output_tokens\",\"message\":\"rejected\"}\n\n", code)
					wantType = "error"
				}
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if channel == "http" {
						w.WriteHeader(http.StatusBadRequest)
					}
					fmt.Fprint(w, body)
				}))
				defer srv.Close()
				p := NewProvider("key", "model", WithBaseURL(srv.URL))
				var err error
				if channel == "failed" || channel == "error" {
					seq, setupErr := p.ChatStream(t.Context(), nil, nil)
					if setupErr != nil {
						t.Fatal(setupErr)
					}
					for _, streamErr := range seq {
						err = streamErr
						break
					}
				} else {
					_, _, err = p.Chat(t.Context(), nil, nil)
				}
				var diagnostic *errorEnvelope
				if !errors.As(err, &diagnostic) || diagnostic.Code != code ||
					diagnostic.Type != wantType || diagnostic.Param != "max_output_tokens" || diagnostic.Message != "rejected" {
					t.Fatalf("structured diagnostic lost: %v (%+v)", err, diagnostic)
				}
				if errors.Is(err, llm.ErrContextOverflow) != (code == "context_length_exceeded") {
					t.Fatalf("wrong overflow classification: %v", err)
				}
				if channel == "http" {
					var api *llm.APIError
					if !errors.As(err, &api) || api.StatusCode != 400 || api.Body != body {
						t.Fatalf("HTTP cause lost: %v", err)
					}
				}
			})
		}
	}
}

func TestContextOverflowHTTPFixtures(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"precise code", 400, `{"error":{"code":"context_length_exceeded","type":"invalid_request_error","param":null}}`, true},
		{"missing code", 400, `{"error":{"type":"context_length_exceeded","message":"context_length_exceeded"}}`, false},
		{"null code", 400, `{"error":{"code":null,"type":"context_length_exceeded"}}`, false},
		{"empty code", 400, `{"error":{"code":"","type":"context_length_exceeded"}}`, false},
		{"output limit", 400, `{"error":{"code":"max_tokens_exceeded","type":"context_length_exceeded","message":"context_length_exceeded"}}`, false},
		{"output parameter", 400, `{"error":{"code":"max_output_tokens","message":"context_length_exceeded"}}`, false},
		{"invalid parameter", 400, `{"error":{"message":"max_output_tokens must be less than 100"}}`, false},
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
		{"top code misplaced", 400, `{"code":"context_length_exceeded"}`, false},
		{"response misplaced", 400, `{"response":{"error":{"code":"context_length_exceeded"}}}`, false},
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

func TestContextOverflowResponseFixtures(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"failed", `{"status":"failed","error":{"code":"context_length_exceeded","message":"too long"}}`, true},
		{"failed no message", `{"status":"failed","error":{"code":"context_length_exceeded"}}`, true},
		{"missing code", `{"status":"failed","error":{"type":"context_length_exceeded","message":"context_length_exceeded"}}`, false},
		{"null code", `{"status":"failed","error":{"code":null,"type":"context_length_exceeded"}}`, false},
		{"empty code", `{"status":"failed","error":{"code":"","type":"context_length_exceeded"}}`, false},
		{"output limit", `{"status":"failed","error":{"code":"max_tokens_exceeded","type":"context_length_exceeded","message":"context_length_exceeded"}}`, false},
		{"output parameter", `{"status":"failed","error":{"code":"max_output_tokens","type":"context_length_exceeded","message":"context_length_exceeded"}}`, false},
		{"top code misplaced", `{"status":"failed","code":"context_length_exceeded"}`, false},
		{"response misplaced", `{"status":"failed","response":{"error":{"code":"context_length_exceeded"}}}`, false},
		{"incomplete tokens", `{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}`, false},
		{"incomplete filter", `{"status":"incomplete","incomplete_details":{"reason":"content_filter"}}`, false},
		{"incomplete distractors", `{"status":"incomplete","code":"context_length_exceeded","type":"context_length_exceeded","message":"context_length_exceeded","error":{"code":"context_length_exceeded","message":"context_length_exceeded"},"incomplete_details":{"reason":"max_output_tokens"}}`, false},
	}
	for _, tt := range tests {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tt.name, stream), func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if stream {
						event := "response.failed"
						if strings.Contains(tt.name, "incomplete") {
							event = "response.incomplete"
						}
						fmt.Fprintf(w, "data: {\"type\":%q,\"response\":%s}\n\n", event, tt.body)
					} else {
						fmt.Fprint(w, tt.body)
					}
				}))
				defer srv.Close()
				p := NewProvider("key", "model", WithBaseURL(srv.URL))
				var err error
				if stream {
					seq, setupErr := p.ChatStream(t.Context(), nil, nil)
					if setupErr != nil {
						t.Fatal(setupErr)
					}
					for _, streamErr := range seq {
						if streamErr != nil {
							err = streamErr
							break
						}
					}
				} else {
					_, _, err = p.Chat(t.Context(), nil, nil)
				}
				if err == nil || errors.Is(err, llm.ErrContextOverflow) != tt.want {
					t.Fatalf("overflow want %t: %v", tt.want, err)
				}
				if tt.want {
					var marker *llm.ContextOverflowError
					if !errors.As(err, &marker) || marker.Code != "context_length_exceeded" || marker.Err == nil {
						t.Fatalf("cause or code lost: %v", err)
					}
				}
				if strings.Contains(tt.name, "incomplete") && !strings.Contains(err.Error(), "incomplete") {
					t.Fatalf("incomplete error changed: %v", err)
				}
			})
		}
	}
}

func TestContextOverflowStreamEventChannels(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    bool
	}{
		{"top error", `{"type":"error","code":"context_length_exceeded","message":"too long"}`, true},
		{"top null code", `{"type":"error","code":null,"message":"context_length_exceeded"}`, false},
		{"top missing code", `{"type":"error","message":"context_length_exceeded"}`, false},
		{"top output code", `{"type":"error","code":"max_tokens_exceeded","message":"context_length_exceeded"}`, false},
		{"top nested misplaced", `{"type":"error","error":{"code":"context_length_exceeded"}}`, false},
		{"top response misplaced", `{"type":"error","response":{"error":{"code":"context_length_exceeded"}}}`, false},
		{"failed top misplaced", `{"type":"response.failed","code":"context_length_exceeded"}`, false},
		{"failed error misplaced", `{"type":"response.failed","error":{"code":"context_length_exceeded"}}`, false},
		{"incomplete all distractors", `{"type":"response.incomplete","code":"context_length_exceeded","error":{"code":"context_length_exceeded"},"response":{"error":{"code":"context_length_exceeded"},"incomplete_details":{"reason":"max_output_tokens"}}}`, false},
		{"malformed", `{"type":"error","code":"context_length_exceeded"`, false},
	}
	p := NewProvider("key", "model")
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := p.parseStreamEvent(tt.payload, map[string]bool{})
			if err == nil || errors.Is(err, llm.ErrContextOverflow) != tt.want {
				t.Fatalf("overflow want %t: %v", tt.want, err)
			}
			if tt.want {
				var marker *llm.ContextOverflowError
				if !errors.As(err, &marker) || marker.Code != "context_length_exceeded" || marker.Err == nil {
					t.Fatalf("cause or code lost: %v", err)
				}
			}
		})
	}
}
