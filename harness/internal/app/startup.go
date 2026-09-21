package app

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/dailz1/go-agent/harness/internal/config"
	"github.com/dailz1/go-agent/harness/internal/prompt"
	"github.com/dailz1/go-agent/harness/internal/workspace"
	"github.com/dailz1/go-agent/llm"
)

// Startup owns startup-only resources. Stage D will consume these in its worker;
// constructing them does not execute a model or create session data.
type Startup struct {
	Config    config.Config
	Provider  llm.Provider
	Approvals *Approvals
	Workspace *workspace.Workspace
	Rules     *prompt.Rules
	Notices   []string
	Logger    *slog.Logger

	logFile *os.File
}

func Prepare(cfg config.Config, getenv func(string) string) (*Startup, error) {
	if cfg.DataDir == "" {
		dir := getenv("XDG_DATA_HOME")
		if dir == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, err
			}
			dir = filepath.Join(home, ".local", "share")
		}
		cfg.DataDir = filepath.Join(dir, "go-agent")
	}
	// The TUI owns the terminal exclusively (DESIGN §2.4): every default
	// logger — the kernel agent and the provider adapters both resolve
	// slog.Default() at construction — must write to a file instead of
	// stderr, or kernel log lines corrupt the rendered screen.
	logDir := filepath.Join(cfg.DataDir, "logs")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}
	logFile, err := os.OpenFile(filepath.Join(logDir, "harness.log"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open harness log: %w", err)
	}
	logger := slog.New(slog.NewTextHandler(logFile, nil))
	slog.SetDefault(logger)
	provider, notices, err := cfg.NewProvider(getenv)
	if err != nil {
		_ = logFile.Close()
		return nil, err
	}
	w, err := workspace.Open(cfg.Workspace, cfg.DataDir)
	if err != nil {
		_ = logFile.Close()
		return nil, err
	}
	cfg.Workspace = w.Path()
	rules, err := prompt.Load(w, cfg.UserRules, 8<<10)
	if err != nil {
		_ = w.Close()
		_ = logFile.Close()
		return nil, err
	}
	return &Startup{
		Config: cfg, Provider: provider, Approvals: NewApprovals(cfg.NoApproval),
		Workspace: w, Rules: rules, Notices: notices, Logger: logger, logFile: logFile,
	}, nil
}

func (s *Startup) Close() error {
	if s.logFile != nil {
		_ = s.logFile.Close()
	}
	return s.Workspace.Close()
}
