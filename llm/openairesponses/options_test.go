package openairesponses

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/llm"
)

// TestOptionsMapping pins the common-option contract: MaxTokens maps to
// max_output_tokens, Temperature to temperature, and WithStop — which the
// protocol cannot express — is rejected explicitly instead of dropped.
func TestOptionsMapping(t *testing.T) {
	var gotBody createRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "resp_1", "status": "completed",
			"output": []map[string]any{{"type": "message", "role": "assistant",
				"content": []map[string]any{{"type": "output_text", "text": "ok"}}}},
		})
	}))
	t.Cleanup(srv.Close)
	p := NewProvider("k", "m", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))

	_, _, err := p.Chat(context.Background(), []llm.Message{llm.UserMessage("hi")}, nil,
		llm.WithMaxTokens(512), llm.WithTemperature(0.3), llm.WithStop("STOP"))
	if !errors.Is(err, ErrStopUnsupported) {
		t.Fatalf("WithStop error = %v, want ErrStopUnsupported", err)
	}
	if gotBody.MaxOutputTokens != 0 {
		t.Errorf("rejected request wrote body with max_output_tokens=%d", gotBody.MaxOutputTokens)
	}

	gotBody = createRequest{}
	if _, _, err := p.Chat(context.Background(), []llm.Message{llm.UserMessage("hi")}, nil,
		llm.WithMaxTokens(512), llm.WithTemperature(0.3)); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if gotBody.MaxOutputTokens != 512 {
		t.Errorf("max_output_tokens = %d, want 512", gotBody.MaxOutputTokens)
	}
	if gotBody.Temperature == nil || *gotBody.Temperature != 0.3 {
		t.Errorf("temperature = %v, want 0.3", gotBody.Temperature)
	}
}

// TestExplicitZeroTemperatureStaysOnWire pins that an explicit
// WithTemperature(0) is serialized: a plain float64 with omitempty would
// silently drop the valid zero value.
func TestExplicitZeroTemperatureStaysOnWire(t *testing.T) {
	var raw []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "resp_1", "status": "completed", "output": []map[string]any{},
		})
	}))
	t.Cleanup(srv.Close)
	p := NewProvider("k", "m", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))

	if _, _, err := p.Chat(context.Background(), []llm.Message{llm.UserMessage("hi")}, nil,
		llm.WithTemperature(0)); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		y := string(raw)
		t.Fatalf("wire body = %s (%v)", y, err)
	}
	temp, present := wire["temperature"]
	if !present {
		t.Fatalf("explicit temperature 0 missing from wire body: %s", string(raw))
	}
	if f, ok := temp.(float64); !ok || f != 0 {
		t.Errorf("wire temperature = %v, want 0", temp)
	}
}

// TestChatRejectsEmptyStatus pins the completed-only gate at its boundary:
// a 200 response with a missing status field is rejected, not returned as
// success.
func TestChatRejectsEmptyStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "resp_1", "output": []map[string]any{},
		})
	}))
	t.Cleanup(srv.Close)
	p := NewProvider("k", "m", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
	_, _, err := p.Chat(context.Background(), []llm.Message{llm.UserMessage("hi")}, nil)
	if err == nil || !strings.Contains(err.Error(), `status ""`) {
		t.Fatalf("Chat error = %v, want an empty-status failure", err)
	}
}

// TestDefaultHTTPClientBounded pins that the default client has a timeout —
// the shared http.DefaultClient has none and can block forever.
func TestDefaultHTTPClientBounded(t *testing.T) {
	p := NewProvider("k", "m")
	if p.httpClient == http.DefaultClient {
		t.Fatal("default provider uses http.DefaultClient (no timeout)")
	}
	if p.httpClient.Timeout <= 0 {
		t.Errorf("default client timeout = %v, want positive", p.httpClient.Timeout)
	}
}

// TestChatRejectsIncompleteStatus pins that a 200 response with a
// non-completed status is an error, not a partial success.
func TestChatRejectsIncompleteStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "resp_1", "status": "incomplete",
			"incomplete_details": map[string]any{"reason": "max_output_tokens"},
			"output": []map[string]any{{"type": "message", "role": "assistant",
				"content": []map[string]any{{"type": "output_text", "text": "partial"}}}},
		})
	}))
	t.Cleanup(srv.Close)
	p := NewProvider("k", "m", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
	_, _, err := p.Chat(context.Background(), []llm.Message{llm.UserMessage("hi")}, nil)
	if err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("Chat error = %v, want an incomplete-status failure", err)
	}
}

// TestChatNon2xxSurfacesAPIError pins the HTTP boundary behavior.
func TestChatNon2xxSurfacesAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
	}))
	t.Cleanup(srv.Close)
	p := NewProvider("k", "m", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
	_, _, err := p.Chat(context.Background(), []llm.Message{llm.UserMessage("hi")}, nil)
	var apiErr *llm.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("error = %v, want *llm.APIError 401", err)
	}
}
