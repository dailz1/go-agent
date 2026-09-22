package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/llm/codex/auth"
)

type testSource struct {
	refreshes atomic.Int32
	refresh   func(*auth.Token) (auth.Token, error)
}

func (s *testSource) Token(context.Context) (auth.Token, error) {
	panic("provider must not call Token")
}
func (s *testSource) Refresh(_ context.Context, rejected *auth.Token) (auth.Token, error) {
	s.refreshes.Add(1)
	if s.refresh != nil {
		return s.refresh(rejected)
	}
	return auth.Token{AccessToken: "token", AccountID: "account"}, nil
}

const emptyCompletion = `{"type":"response.completed","response":{"status":"completed","output":[]}}`

func TestProviderLazyWireAndOutputLimit(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/responses" || r.Header.Get("Authorization") != "Bearer token" ||
			r.Header.Get("ChatGPT-Account-Id") != "account" || r.Header.Get("Originator") != "go_agent" ||
			r.Header.Get("User-Agent") != "go-agent/codex" {
			t.Errorf("wrong request headers/path: %s %v", r.URL, r.Header)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		if body["model"] != "override" || body["instructions"] != "" || body["stream"] != true || body["store"] != false {
			t.Errorf("body=%v", body)
		}
		for _, name := range []string{"max_tokens", "max_output_tokens", "previous_response_id"} {
			if _, exists := body[name]; exists {
				t.Errorf("unexpected %s", name)
			}
		}
		fmt.Fprint(w, sse(emptyCompletion))
	}))
	defer server.Close()
	source := &testSource{}
	var logs bytes.Buffer
	provider := NewProvider("model", WithAuthSource(source), WithBaseURL(server.URL+"/responses/"), WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))))
	for _, limit := range []int{0, 4096, 8} {
		before := requests.Load()
		logSize := logs.Len()
		seq, err := provider.ChatStream(t.Context(), nil, nil, llm.WithMaxTokens(limit), llm.WithModel("override"))
		if err != nil {
			t.Fatal(err)
		}
		if source.refreshes.Load() != before || requests.Load() != before || logs.Len() != logSize {
			t.Fatal("eager refresh, request, or warning")
		}
		for _, err := range seq {
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if requests.Load() != 3 || source.refreshes.Load() != 3 {
		t.Fatal("unexpected requests")
	}
	if strings.Count(logs.String(), `"output_limit_mode":"server_default"`) != 1 {
		t.Fatalf("warnings: %s", logs.String())
	}
}

func TestProviderConfiguration(t *testing.T) {
	for name, options := range map[string][]ProviderOption{
		"missing source":   {},
		"bad url":          {WithAuthSource(&testSource{}), WithBaseURL("https://user:pass@example.com")},
		"query":            {WithAuthSource(&testSource{}), WithBaseURL("https://example.com?q=x")},
		"plaintext remote": {WithAuthSource(&testSource{}), WithBaseURL("http://example.com")},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewProvider("m", options...).ChatStream(t.Context(), nil, nil); err == nil {
				t.Fatal("accepted configuration")
			}
		})
	}
	for name, option := range map[string]llm.Option{"negative": llm.WithMaxTokens(-1), "temperature": llm.WithTemperature(0), "stop": llm.WithStop("x")} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewProvider("m", WithAuthSource(&testSource{})).ChatStream(t.Context(), nil, nil, option); err == nil {
				t.Fatal("accepted option")
			}
		})
	}
}

func TestProviderHTTPErrorMatrix(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		code, kind string
		retry      bool
	}{
		{"quota", 429, "usage_limit_reached", KindQuota, false},
		{"rate", 429, "rate_limit_exceeded", "", true},
		{"unknown rate", 429, "unknown", "", true},
		{"server", 503, "server_error", "", true},
		{"bad request", 400, "unsupported_parameter", KindCapability, false},
		{"forbidden", 403, "denied", KindCapability, false},
		{"unauthorized", 401, "invalid_token", KindAuth, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("Retry-After", "2")
				w.WriteHeader(tc.status)
				fmt.Fprintf(w, `{"error":{"code":%q,"resets_at":2000000000}}`, tc.code)
			}))
			defer server.Close()
			source := &testSource{}
			_, _, err := NewProvider("m", WithAuthSource(source), WithBaseURL(server.URL)).Chat(t.Context(), nil, nil)
			var api *llm.APIError
			if !errors.As(err, &api) || api.StatusCode != tc.status || api.Retryable() != tc.retry || api.RetryAfter != 2e9 {
				t.Fatalf("error=%v api=%+v", err, api)
			}
			var typed *Error
			if tc.kind != "" && (!errors.As(err, &typed) || typed.Kind != tc.kind || typed.Code != tc.code) {
				t.Fatalf("classification=%v", err)
			}
			want := int32(1)
			if tc.status == 401 {
				want = 2
			}
			if requests.Load() != want || source.refreshes.Load() != want {
				t.Fatal("unexpected retries")
			}
		})
	}
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type trackedBody struct {
	io.Reader
	closed bool
}

func (b *trackedBody) Close() error { b.closed = true; return nil }

func TestProviderBreakClosesBody(t *testing.T) {
	body := &trackedBody{Reader: strings.NewReader(sse(`{"type":"response.output_text.delta","output_index":0,"content_index":0,"item_id":"m","delta":"x"}`))}
	client := &http.Client{Transport: roundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: body, Header: make(http.Header)}, nil
	})}
	seq, err := NewProvider("m", WithAuthSource(&testSource{}), WithHTTPClient(client)).ChatStream(t.Context(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range seq {
		if err != nil {
			t.Fatal(err)
		}
		break
	}
	if !body.closed {
		t.Fatal("body left open")
	}
}

func TestProviderWirePreservesSeparateMessages(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input []inputItem `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if len(request.Input) != 2 || len(request.Input[0].Content) != 1 || len(request.Input[1].Content) != 1 {
			t.Errorf("message boundaries lost: %+v", request.Input)
		}
		fmt.Fprint(w, sse(emptyCompletion))
	}))
	defer server.Close()
	p := NewProvider("m", WithAuthSource(&testSource{}), WithBaseURL(server.URL))
	if _, _, err := p.Chat(t.Context(), []llm.Message{llm.UserMessage("a"), llm.UserMessage("b")}, nil); err != nil {
		t.Fatal(err)
	}
}
