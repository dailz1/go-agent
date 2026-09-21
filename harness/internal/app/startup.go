package app

import (
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
}

func Prepare(cfg config.Config, getenv func(string) string) (*Startup, error) {
	provider, notices, err := cfg.NewProvider(getenv)
	if err != nil {
		return nil, err
	}
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
	w, err := workspace.Open(cfg.Workspace, cfg.DataDir)
	if err != nil {
		return nil, err
	}
	cfg.Workspace = w.Path()
	rules, err := prompt.Load(w, cfg.UserRules, 8<<10)
	if err != nil {
		w.Close()
		return nil, err
	}
	return &Startup{
		Config: cfg, Provider: provider, Approvals: NewApprovals(cfg.NoApproval),
		Workspace: w, Rules: rules, Notices: notices,
	}, nil
}

func (s *Startup) Close() error { return s.Workspace.Close() }
