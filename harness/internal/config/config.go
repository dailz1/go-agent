// Package config loads startup-only options and constructs provider adapters.
package config

import (
	"errors"
	"flag"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Config contains non-secret startup settings. Credentials are never retained here.
type Config struct {
	Provider         string `json:"provider"`
	Model            string `json:"model"`
	BaseURL          string `json:"base_url"`
	APIKeyEnv        string `json:"api_key_env"`
	Workspace        string `json:"workspace"`
	UserRules        string `json:"user_rules"`
	DataDir          string `json:"data_dir"`
	Excludes         string `json:"excludes"`
	MaxIterations    int    `json:"max_iterations"`
	ContextBudget    int    `json:"context_budget"`
	MaxOutputTokens  int    `json:"max_output_tokens"`
	RunTimeout       string `json:"run_timeout"`
	SnapshotQuotaMiB int    `json:"snapshot_quota"`
	NoApproval       bool   `json:"-"`
}

// Parse applies flags over non-secret environment defaults. Missing model and
// credentials do not prevent help or non-terminal startup.
func Parse(args []string, getenv func(string) string) (Config, error) {
	cfg := defaults()
	configPath := getenv("GO_AGENT_CONFIG")
	explicit := configPath != ""
	if !explicit {
		dir, err := os.UserConfigDir()
		if err != nil {
			return Config{}, err
		}
		configPath = filepath.Join(dir, "go-agent", "config.json")
	}
	// A preliminary parse resolves only the config path. Help never reads disk.
	pre := flags(&cfg)
	pre.StringVar(&configPath, "config", configPath, "user JSON configuration")
	if err := pre.Parse(args); err != nil {
		return Config{}, err
	}
	pre.Visit(func(f *flag.Flag) {
		if f.Name == "config" {
			explicit = true
		}
	})
	cfg = defaults()
	if err := loadJSON(configPath, explicit, &cfg); err != nil {
		return Config{}, err
	}
	if err := applyEnv(&cfg, getenv); err != nil {
		return Config{}, err
	}
	fs := flags(&cfg)
	fs.StringVar(&configPath, "config", configPath, "user JSON configuration")
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	if fs.NArg() != 0 {
		return Config{}, errors.New("positional arguments are not supported")
	}
	switch cfg.Provider {
	case "openai", "openai_responses", "glm":
	default:
		return Config{}, errors.New("provider must be openai, openai_responses, or glm")
	}
	if cfg.APIKeyEnv == "" {
		cfg.APIKeyEnv = "OPENAI_API_KEY"
		if cfg.Provider == "glm" {
			cfg.APIKeyEnv = "GLM_API_KEY"
		}
	}
	if !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`).MatchString(cfg.APIKeyEnv) {
		return Config{}, errors.New("api-key-env must name an environment variable")
	}
	if cfg.BaseURL != "" {
		u, err := url.Parse(cfg.BaseURL)
		if err != nil {
			return Config{}, errors.New("base-url must be an HTTP(S) endpoint")
		}
		httpScheme := u.Scheme == "http" || u.Scheme == "https"
		if !httpScheme || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return Config{}, errors.New("base-url must be an HTTP(S) endpoint")
		}
	}
	if err := cfg.validateLimits(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Help lists the shipped flags without including environment values.
func Help() string {
	var text strings.Builder
	text.WriteString("Usage: go-agent [options]\n\nStage D startup; the durable session controller is wired (sessions, cancel-and-settle,\nredirect), the interactive chat surface arrives in Stage E and no model is called at startup.\n")
	text.WriteString("Non-TTY input/output prints help and exits. In a TTY: q, Esc, Ctrl+C quit.\n\n")
	cfg := defaults()
	fs := flags(&cfg)
	fs.String("config", "", "user JSON configuration (GO_AGENT_CONFIG)")
	fs.SetOutput(&text)
	fs.PrintDefaults()
	return text.String()
}

func flags(cfg *Config) *flag.FlagSet {
	fs := flag.NewFlagSet("go-agent", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(
		&cfg.Provider,
		"provider",
		cfg.Provider,
		"openai, openai_responses, or glm (GO_AGENT_PROVIDER)",
	)
	fs.StringVar(
		&cfg.Model,
		"model",
		cfg.Model,
		"model name (GO_AGENT_MODEL); required for TTY startup",
	)
	fs.StringVar(
		&cfg.BaseURL,
		"base-url",
		cfg.BaseURL,
		"HTTP(S) endpoint (GO_AGENT_BASE_URL)",
	)
	fs.StringVar(
		&cfg.APIKeyEnv,
		"api-key-env",
		cfg.APIKeyEnv,
		"credential variable name (GO_AGENT_API_KEY_ENV); defaults to OPENAI_API_KEY or GLM_API_KEY",
	)
	fs.BoolVar(
		&cfg.NoApproval,
		"no-approval",
		false,
		"explicit approval bypass; cannot be set via environment",
	)
	fs.StringVar(&cfg.Workspace, "workspace", cfg.Workspace, "workspace root (GO_AGENT_WORKSPACE)")
	fs.StringVar(&cfg.UserRules, "user-rules", cfg.UserRules, "explicit user instructions file (GO_AGENT_USER_RULES)")
	fs.StringVar(&cfg.DataDir, "data-dir", cfg.DataDir, "private harness data directory (GO_AGENT_DATA_DIR)")
	fs.StringVar(&cfg.Excludes, "excludes", cfg.Excludes, "comma-separated excluded search directories (GO_AGENT_EXCLUDES)")
	fs.IntVar(&cfg.MaxIterations, "max-iterations", cfg.MaxIterations, "maximum model rounds (GO_AGENT_MAX_ITERATIONS)")
	fs.IntVar(&cfg.ContextBudget, "context-budget", cfg.ContextBudget, "heuristic context budget (GO_AGENT_CONTEXT_BUDGET)")
	fs.IntVar(&cfg.MaxOutputTokens, "max-output-tokens", cfg.MaxOutputTokens, "output token budget (GO_AGENT_MAX_OUTPUT_TOKENS)")
	fs.StringVar(&cfg.RunTimeout, "run-timeout", cfg.RunTimeout, "run duration (GO_AGENT_RUN_TIMEOUT)")
	fs.IntVar(&cfg.SnapshotQuotaMiB, "snapshot-quota", cfg.SnapshotQuotaMiB, "per-session snapshot budget in MiB (GO_AGENT_SNAPSHOT_QUOTA)")
	fs.Usage = func() {}
	return fs
}

// ValidateInteractive checks requirements that do not apply to help.
func (c Config) ValidateInteractive() error {
	if strings.TrimSpace(c.Model) == "" {
		return errors.New("model is required: use --model or GO_AGENT_MODEL")
	}
	return nil
}
