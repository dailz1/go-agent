// Package config parses startup options without reading provider credentials.
package config

import (
	"errors"
	"flag"
	"io"
	"net/url"
	"regexp"
	"strings"
)

// Config is the Stage A startup surface, not a persisted session configuration.
type Config struct {
	Provider   string
	Model      string
	BaseURL    string
	APIKeyEnv  string
	NoApproval bool
}

// Parse applies flags over non-secret environment defaults. Missing model and
// credentials do not prevent help or non-terminal startup.
func Parse(args []string, getenv func(string) string) (Config, error) {
	cfg := Config{
		Provider:  getenv("GO_AGENT_PROVIDER"),
		Model:     getenv("GO_AGENT_MODEL"),
		BaseURL:   getenv("GO_AGENT_BASE_URL"),
		APIKeyEnv: getenv("GO_AGENT_API_KEY_ENV"),
	}
	if cfg.Provider == "" {
		cfg.Provider = "openai"
	}
	fs := flags(&cfg)
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
		if !httpScheme || u.Hostname() == "" {
			return Config{}, errors.New("base-url must be an HTTP(S) endpoint")
		}
	}
	// TODO(B): load user JSON, resolve credentials and construct the provider.
	return cfg, nil
}

// Help lists the shipped flags without including environment values.
func Help() string {
	var text strings.Builder
	text.WriteString("Usage: go-agent [options]\n\nStage A skeleton; no model or tool execution.\n")
	text.WriteString("Non-TTY input/output prints help and exits. In a TTY: q, Esc, Ctrl+C quit.\n\n")
	cfg := Config{Provider: "openai"}
	fs := flags(&cfg)
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
