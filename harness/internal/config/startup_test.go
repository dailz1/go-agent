package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStartupPrecedence(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "config.json")
	if err := os.WriteFile(file, []byte(`{"provider":"glm","model":"json","workspace":"`+dir+`","max_iterations":12}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Parse([]string{"--config", file, "--model", "flag"}, func(k string) string {
		return map[string]string{"GO_AGENT_MODEL": "env", "GO_AGENT_MAX_ITERATIONS": "14"}[k]
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model != "flag" || cfg.Provider != "glm" || cfg.Workspace != dir || cfg.MaxIterations != 14 {
		t.Fatalf("config precedence: %+v", cfg)
	}
}

func TestStartupRejectsConfigSecretsAndInvalidBudgets(t *testing.T) {
	for _, body := range []string{`null`, `{"api_key":"secret"}`, `{"no_approval":true}`, `{"model":"x"} {}`, `{"max_iterations":0}`} {
		t.Run(body, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(file, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Parse([]string{"--config", file}, func(string) string { return "" }); err == nil {
				t.Fatal("accepted invalid configuration")
			}
		})
	}
}

func TestProviderCredentialsAreNotPersisted(t *testing.T) {
	for _, name := range []string{"openai", "openai_responses", "glm"} {
		t.Run(name, func(t *testing.T) {
			cfg, err := Parse([]string{"--provider", name, "--model", "m"}, func(string) string { return "" })
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := cfg.NewProvider(func(string) string { return "" }); err == nil {
				t.Fatal("missing credential accepted")
			}
			p, _, err := cfg.NewProvider(func(string) string { return "secret" })
			if err != nil || p.Name() != name {
				t.Fatalf("provider = %v, err = %v", p, err)
			}
			data, err := json.Marshal(cfg)
			if err != nil || strings.Contains(string(data), "secret") {
				t.Fatalf("config serialization = %s, %v", data, err)
			}
		})
	}
}
