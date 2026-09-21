package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/llm/glm"
	"github.com/dailz1/go-agent/llm/openai"
	"github.com/dailz1/go-agent/llm/openairesponses"
)

func defaults() Config {
	return Config{
		Provider: "openai", Workspace: ".", Excludes: ".git,node_modules,vendor,build,dist,target",
		MaxIterations: 30, ContextBudget: 8192, MaxOutputTokens: 4096, RunTimeout: "15m",
		SnapshotQuotaMiB: 256,
	}
}

func loadJSON(path string, explicit bool, cfg *Config) error {
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) && !explicit {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open user configuration: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 64*1024+1))
	if err != nil {
		return fmt.Errorf("read user configuration: %w", err)
	}
	if len(data) > 64*1024 {
		return errors.New("user configuration exceeds 64 KiB")
	}
	if !strings.HasPrefix(strings.TrimSpace(string(data)), "{") {
		return errors.New("user configuration must contain one JSON object")
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(cfg); err != nil {
		// Do not echo invalid JSON: it may contain accidentally pasted secrets.
		return errors.New("invalid user configuration JSON or unsupported field")
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return errors.New("user configuration must contain one JSON object")
	}
	return nil
}

func applyEnv(cfg *Config, getenv func(string) string) error {
	for key, dst := range map[string]*string{
		"PROVIDER": &cfg.Provider, "MODEL": &cfg.Model, "BASE_URL": &cfg.BaseURL,
		"API_KEY_ENV": &cfg.APIKeyEnv, "WORKSPACE": &cfg.Workspace, "USER_RULES": &cfg.UserRules,
		"DATA_DIR": &cfg.DataDir, "EXCLUDES": &cfg.Excludes, "RUN_TIMEOUT": &cfg.RunTimeout,
	} {
		if value := getenv("GO_AGENT_" + key); value != "" {
			*dst = value
		}
	}
	for key, dst := range map[string]*int{
		"MAX_ITERATIONS": &cfg.MaxIterations, "CONTEXT_BUDGET": &cfg.ContextBudget,
		"MAX_OUTPUT_TOKENS": &cfg.MaxOutputTokens, "SNAPSHOT_QUOTA": &cfg.SnapshotQuotaMiB,
	} {
		if value := getenv("GO_AGENT_" + key); value != "" {
			n, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("GO_AGENT_%s must be an integer", key)
			}
			*dst = n
		}
	}
	return nil
}

func (c Config) validateLimits() error {
	if c.MaxIterations <= 0 || c.ContextBudget <= 0 || c.MaxOutputTokens <= 0 {
		return errors.New("iteration and token budgets must be positive")
	}
	if c.SnapshotQuotaMiB <= 0 {
		return errors.New("snapshot-quota must be positive")
	}
	d, err := time.ParseDuration(c.RunTimeout)
	if err != nil || d <= 0 {
		return errors.New("run-timeout must be a positive duration")
	}
	if c.Workspace == "" {
		return errors.New("workspace must not be empty")
	}
	return nil
}

// SnapshotQuotaBytes converts the startup budget for the session snapshot store.
func (c Config) SnapshotQuotaBytes() int64 {
	return int64(c.SnapshotQuotaMiB) << 20
}

// NewProvider resolves the key once, without storing it in Config or diagnostics.
// The second result contains non-secret startup notices.
func (c Config) NewProvider(getenv func(string) string) (llm.Provider, []string, error) {
	if err := c.ValidateInteractive(); err != nil {
		return nil, nil, err
	}
	key := getenv(c.APIKeyEnv)
	if key == "" {
		return nil, nil, fmt.Errorf("credential environment variable %s is empty", c.APIKeyEnv)
	}
	notices := []string{}
	switch c.Provider {
	case "openai":
		opts := []openai.ProviderOption{}
		if c.BaseURL != "" {
			opts = append(opts, openai.WithBaseURL(c.BaseURL))
		}
		return openai.NewProvider(key, c.Model, opts...), notices, nil
	case "openai_responses":
		opts := []openairesponses.ProviderOption{}
		if c.BaseURL != "" {
			opts = append(opts, openairesponses.WithBaseURL(c.BaseURL))
		}
		return openairesponses.NewProvider(key, c.Model, opts...), notices, nil
	case "glm":
		opts := []glm.ProviderOption{}
		if c.BaseURL != "" {
			opts = append(opts, glm.WithBaseURL(c.BaseURL))
		}
		if strings.Contains(key, ".") {
			notices = append(notices, "GLM key contains a dot: the adapter uses JWT authentication.")
		}
		return glm.NewProvider(key, c.Model, opts...), notices, nil
	default:
		return nil, nil, errors.New("unsupported provider")
	}
}
