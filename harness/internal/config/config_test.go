package config

import (
	"errors"
	"flag"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name string
		args []string
		env  map[string]string
		want Config
	}{
		{
			name: "defaults",
			want: Config{Provider: "openai", APIKeyEnv: "OPENAI_API_KEY"},
		},
		{
			name: "environment",
			env: map[string]string{
				"GO_AGENT_PROVIDER": "glm",
				"GO_AGENT_MODEL":    "env-model",
				"GO_AGENT_BASE_URL": "https://env.example/v1",
			},
			want: Config{
				Provider: "glm", Model: "env-model",
				BaseURL: "https://env.example/v1", APIKeyEnv: "GLM_API_KEY",
			},
		},
		{
			name: "flags override environment",
			args: []string{
				"--provider", "openai_responses", "--model", "flag-model",
				"--base-url", "http://localhost:8080/v1", "--api-key-env", "CUSTOM_KEY",
			},
			env: map[string]string{
				"GO_AGENT_PROVIDER":    "glm",
				"GO_AGENT_MODEL":       "env-model",
				"GO_AGENT_BASE_URL":    "https://env.example/v1",
				"GO_AGENT_API_KEY_ENV": "ENV_KEY",
			},
			want: Config{
				Provider: "openai_responses", Model: "flag-model",
				BaseURL: "http://localhost:8080/v1", APIKeyEnv: "CUSTOM_KEY",
			},
		},
		{
			name: "environment cannot disable approval",
			env:  map[string]string{"GO_AGENT_NO_APPROVAL": "true"},
			want: Config{Provider: "openai", APIKeyEnv: "OPENAI_API_KEY"},
		},
		{
			name: "explicit approval bypass",
			args: []string{"--no-approval"},
			want: Config{Provider: "openai", APIKeyEnv: "OPENAI_API_KEY", NoApproval: true},
		},
		{
			name: "snapshot quota flag and env",
			args: []string{"--snapshot-quota", "64"},
			env:  map[string]string{"GO_AGENT_SNAPSHOT_QUOTA": "128"},
			want: Config{Provider: "openai", APIKeyEnv: "OPENAI_API_KEY", SnapshotQuotaMiB: 64},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.want.Workspace = "."
			tt.want.Excludes = ".git,node_modules,vendor,build,dist,target"
			tt.want.MaxIterations = 30
			tt.want.ContextBudget = 8192
			tt.want.MaxOutputTokens = 4096
			tt.want.RunTimeout = "15m"
			tt.want.SnapshotQuotaMiB = 256
			if tt.name == "snapshot quota flag and env" {
				tt.want.SnapshotQuotaMiB = 64
			}
			got, err := Parse(tt.args, func(key string) string { return tt.env[key] })
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("Parse() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParseRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "unknown flag", args: []string{"--unknown"}},
		{name: "positional task", args: []string{"run a task"}},
		{name: "unknown provider", args: []string{"--provider", "other"}},
		{name: "invalid endpoint", args: []string{"--base-url", "not-a-url"}},
		{name: "non HTTP endpoint", args: []string{"--base-url", "file:///tmp/model"}},
		{name: "invalid key variable", args: []string{"--api-key-env", "not=a=name"}},
		{name: "key value flag forbidden", args: []string{"--api-key", "secret"}},
		{name: "invalid boolean", args: []string{"--no-approval=perhaps"}},
		{name: "invalid snapshot quota", args: []string{"--snapshot-quota", "0"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := Parse(tt.args, func(string) string { return "" }); err == nil {
				t.Fatal("Parse() accepted invalid input")
			}
		})
	}
}

func TestParseHelpDoesNotRequireConfiguration(t *testing.T) {
	for _, arg := range []string{"-h", "--help"} {
		t.Run(arg, func(t *testing.T) {
			_, err := Parse([]string{arg}, func(string) string { return "" })
			if !errors.Is(err, flag.ErrHelp) {
				t.Fatalf("Parse() error = %v, want flag.ErrHelp", err)
			}
		})
	}
}

func TestParseDoesNotReadSecrets(t *testing.T) {
	_, err := Parse([]string{"--api-key-env", "SECRET_KEY"}, func(key string) string {
		if !strings.HasPrefix(key, "GO_AGENT_") {
			t.Fatalf("secret lookup during stub parsing: %s", key)
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
}
