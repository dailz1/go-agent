package glm

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
	const body = `{"error":{"code":"1261"}}`
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
	const body = "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"model_context_window_exceeded\"}]}\n"
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
		{"complete", "data: {\"choices\":[{\"finish_reason\":\"model_context_window_exceeded\"}]}\n\n", true},
		{"clean eof", "data: {\"choices\":[{\"finish_reason\":\"model_context_window_exceeded\"}]}\n", false},
		{"split string", "data: {\"choices\":[{\"finish_reason\":\"model_context_window_\ndata: exceeded\"}]}\n\n", false},
		{"valid multiline", "data: {\"choices\":[{\ndata: \"finish_reason\":\"model_context_window_exceeded\"}]}\n\n", true},
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

func TestContextOverflowHTTPFixtures(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"string code", 400, `{"error":{"code":"1261"}}`, true},
		{"numeric code", 400, `{"error":{"code":1261}}`, true},
		{"substring code", 400, `{"error":{"code":"error 1261"}}`, false},
		{"fractional code", 400, `{"error":{"code":1261.5}}`, false},
		{"missing code", 400, `{"error":{"type":"1261","message":"context_length_exceeded"}}`, false},
		{"null code", 400, `{"error":{"code":null,"type":"context_length_exceeded"}}`, false},
		{"empty code", 400, `{"error":{"code":"","type":"context_length_exceeded"}}`, false},
		{"output limit", 400, `{"error":{"code":"max_tokens_exceeded","type":"context_length_exceeded","message":"1261 context_length_exceeded"}}`, false},
		{"output parameter", 400, `{"error":{"code":"max_output_tokens","message":"context_length_exceeded"}}`, false},
		{"invalid parameter", 400, `{"error":{"message":"max_tokens must be less than 100"}}`, false},
		{"rate limit", 429, `{"error":{"code":"rate_limit_exceeded","message":"too many tokens"}}`, false},
		{"quota", 429, `{"error":{"code":"insufficient_quota"}}`, false},
		{"usage limit", 429, `{"error":{"code":"usage_limit_reached"}}`, false},
		{"server error", 500, `{"error":{"code":"1261","message":"context_length_exceeded"}}`, false},
		{"unauthorized", 401, `{"error":{"code":"1261","message":"context_length_exceeded"}}`, false},
		{"forbidden", 403, `{"error":{"code":"1261","message":"context_length_exceeded"}}`, false},
		{"large empty", 413, "", false},
		{"large html", 413, "<html>1261 context_length_exceeded</html>", false},
		{"invalid request", 400, `{"error":{"type":"invalid_request"}}`, false},
		{"malformed", 400, `{"error":{"code":"1261"}`, false},
		{"wrong channel", 400, `{"code":"1261"}`, false},
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
					if !errors.As(err, &marker) || marker.Code != "1261" || api.Retryable() {
						t.Fatalf("invalid marker: %v", err)
					}
				}
			})
		}
	}
}

func TestContextOverflowFinishFixtures(t *testing.T) {
	for _, reason := range []string{"model_context_window_exceeded", "length", "network_error"} {
		for _, text := range []string{"", "partial"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", reason, text, stream), func(t *testing.T) {
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if stream {
							w.Header().Set("Content-Type", "text/event-stream")
							fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q},\"finish_reason\":%q}]}\n\n", text, reason)
						} else {
							fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":%q},"finish_reason":%q}]}`, text, reason)
						}
					}))
					defer srv.Close()
					p := NewProvider("key", "model", WithBaseURL(srv.URL))
					var err error
					var chunks []llm.Chunk
					if stream {
						seq, setupErr := p.ChatStream(t.Context(), nil, nil)
						if setupErr != nil {
							t.Fatal(setupErr)
						}
						for chunk, streamErr := range seq {
							if streamErr != nil {
								err = streamErr
								break
							}
							chunks = append(chunks, chunk)
						}
					} else {
						_, _, err = p.Chat(t.Context(), nil, nil)
					}
					want := reason == "model_context_window_exceeded"
					if errors.Is(err, llm.ErrContextOverflow) != want {
						t.Fatalf("overflow=%t, want %t: %v", errors.Is(err, llm.ErrContextOverflow), want, err)
					}
					if reason == "length" {
						if !stream && text == "" {
							// GLM rejects an empty non-streaming message through
							// its existing conversion rule, without an overflow marker.
							if err == nil {
								t.Fatal("empty message accepted")
							}
						} else if err != nil {
							t.Fatalf("length returned error: %v", err)
						}
						return
					}
					var api *llm.APIError
					if !errors.As(err, &api) || api.Retryable() != (reason == "network_error") {
						t.Fatalf("finish cause changed: %v", err)
					}
					if want {
						var marker *llm.ContextOverflowError
						if !errors.As(err, &marker) || marker.Code != reason || len(chunks) != 0 {
							t.Fatalf("finish marker or payload chunk delivery changed: %v %v", err, chunks)
						}
					}
				})
			}
		}
	}
}

func TestContextOverflowSamePayloadDelivery(t *testing.T) {
	for _, prior := range []bool{false, true} {
		t.Run(fmt.Sprintf("prior=%t", prior), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if prior {
					fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"earlier\"}}]}\n\n")
				}
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"undelivered\"},\"finish_reason\":\"model_context_window_exceeded\"}]}\n\n")
			}))
			defer srv.Close()
			p := NewProvider("key", "model", WithBaseURL(srv.URL))
			seq, err := p.ChatStream(t.Context(), nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			var text string
			for chunk, streamErr := range seq {
				if streamErr != nil {
					err = streamErr
					break
				}
				if delta, ok := chunk.(*llm.TextDeltaChunk); ok {
					text += delta.Text
				}
			}
			want := ""
			if prior {
				want = "earlier"
			}
			if text != want || !errors.Is(err, llm.ErrContextOverflow) {
				t.Fatalf("text=%q, want %q, err=%v", text, want, err)
			}
		})
	}
}
