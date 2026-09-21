package config

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dailz1/go-agent/llm"
)

func TestProviderUsesStartupEndpointModelAndKey(t *testing.T) {
	for _, name := range []string{"openai", "openai_responses", "glm"} {
		t.Run(name, func(t *testing.T) {
			requests := make(chan map[string]string, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Model string `json:"model"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				requests <- map[string]string{"model": body.Model, "auth": r.Header.Get("Authorization"), "path": r.URL.Path}
				w.Header().Set("Content-Type", "application/json")
				result := `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`
				if name == "openai_responses" {
					result = `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`
				}
				if _, err := w.Write([]byte(result)); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			cfg, err := Parse([]string{"--provider", name, "--model", "chosen", "--base-url", server.URL}, func(string) string { return "" })
			if err != nil {
				t.Fatal(err)
			}
			provider, _, err := cfg.NewProvider(func(string) string { return "startup-secret" })
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := provider.Chat(t.Context(), []llm.Message{llm.UserMessage("ping")}, nil); err != nil {
				t.Fatal(err)
			}
			got := <-requests
			wantPath := "/chat/completions"
			if name == "openai_responses" {
				wantPath = "/responses"
			}
			if got["model"] != "chosen" || got["auth"] != "Bearer startup-secret" || got["path"] != wantPath {
				t.Fatalf("startup request fields do not match configuration: model=%q path=%q", got["model"], got["path"])
			}
		})
	}
}
