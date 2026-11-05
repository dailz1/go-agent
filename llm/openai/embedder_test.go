package openai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dailz1/go-agent/llm"
)

func TestEmbedAlignsVectorsByResponseIndex(t *testing.T) {
	var reqBody embeddingsRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
			t.Errorf("decode request: %v", err)
		}
		// Respond deliberately out of order: the adapter must reorder by
		// the index field, not by arrival order.
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"index": 2, "embedding": []float32{7, 8, 9}},
				{"index": 0, "embedding": []float32{1, 2, 3}},
				{"index": 1, "embedding": []float32{4, 5, 6}},
			},
			"usage": map[string]any{"prompt_tokens": 42, "total_tokens": 42},
		})
	}))
	defer srv.Close()

	e := NewEmbedder("key", "text-embedding-3-small", WithBaseURL(srv.URL))
	got, usage, err := e.Embed(context.Background(), []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	want := [][]float32{{1, 2, 3}, {4, 5, 6}, {7, 8, 9}}
	for i := range want {
		if len(got[i]) != 3 {
			t.Fatalf("vector %d: got %d dims, want 3", i, len(got[i]))
		}
		for j := range want[i] {
			if got[i][j] != want[i][j] {
				t.Fatalf("vector %d[%d]: got %v, want %v", i, j, got[i], want[i])
			}
		}
	}
	if usage.InputTokens != 42 {
		t.Fatalf("usage.InputTokens = %d, want 42", usage.InputTokens)
	}
	if reqBody.Model != "text-embedding-3-small" {
		t.Errorf("request model = %q", reqBody.Model)
	}
	if reqBody.Dimensions != 0 {
		t.Errorf("dimensions emitted without WithDimensions: %d", reqBody.Dimensions)
	}
}

func TestEmbedSendsDimensions(t *testing.T) {
	var reqBody embeddingsRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&reqBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1]}],"usage":{"prompt_tokens":1}}`))
	}))
	defer srv.Close()

	e := NewEmbedder("key", "m", WithBaseURL(srv.URL))
	if _, _, err := e.Embed(context.Background(), []string{"a"}, llm.WithEmbedModel("text-embedding-3-large"), llm.WithDimensions(256)); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if reqBody.Model != "text-embedding-3-large" {
		t.Errorf("model = %q, want per-call override", reqBody.Model)
	}
	if reqBody.Dimensions != 256 {
		t.Errorf("dimensions = %d, want 256", reqBody.Dimensions)
	}
}

func TestEmbedErrorCases(t *testing.T) {
	t.Run("empty input makes no HTTP call", func(t *testing.T) {
		called := false
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
		}))
		defer srv.Close()
		e := NewEmbedder("key", "m", WithBaseURL(srv.URL))
		if _, _, err := e.Embed(context.Background(), nil); err == nil {
			t.Fatal("want error for empty input")
		}
		if called {
			t.Error("empty input must not hit the endpoint")
		}
	})

	t.Run("non-2xx is retryable APIError", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
		}))
		defer srv.Close()
		e := NewEmbedder("key", "m", WithBaseURL(srv.URL))
		_, _, err := e.Embed(context.Background(), []string{"a"})
		var apiErr *llm.APIError
		if !errors.As(err, &apiErr) {
			t.Fatalf("want *llm.APIError, got %T: %v", err, err)
		}
		if apiErr.StatusCode != 429 || !apiErr.Retryable() {
			t.Errorf("status=%d retryable=%v, want 429/true", apiErr.StatusCode, apiErr.Retryable())
		}
	})

	t.Run("missing index is a typed error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1]}],"usage":{}}`))
		}))
		defer srv.Close()
		e := NewEmbedder("key", "m", WithBaseURL(srv.URL))
		if _, _, err := e.Embed(context.Background(), []string{"a", "b"}); err == nil {
			t.Fatal("want error when response covers fewer inputs than sent")
		}
	})

	t.Run("duplicate index is a typed error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1]},{"index":0,"embedding":[2]}],"usage":{}}`))
		}))
		defer srv.Close()
		e := NewEmbedder("key", "m", WithBaseURL(srv.URL))
		if _, _, err := e.Embed(context.Background(), []string{"a", "b"}); err == nil {
			t.Fatal("want error on duplicate response index")
		}
	})
}
