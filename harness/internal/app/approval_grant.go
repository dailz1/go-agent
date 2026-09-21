package app

import (
	"errors"
	"path/filepath"
	"strings"
	"time"

	"github.com/dailz1/go-agent/harness/internal/workspace"
)

type fileGrant struct {
	thread    string
	workspace string
	paths     map[string]bool
	remaining int
	expires   time.Time
}

// Grant is an explicit host action; it never answers an existing request.
func (a *Approvals) Grant(token string, paths []string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	r := a.active
	if r == nil || r.token != token {
		return errors.New("grant has no matching run")
	}
	if err := r.check(r.ctx); err != nil {
		return err
	}
	if len(paths) == 0 {
		return errors.New("grant requires an exact file set")
	}
	exact := make(map[string]bool, len(paths))
	for _, path := range paths {
		if err := r.workspace.Writable(path); err != nil {
			return err
		}
		if !grantable(path) {
			return errors.New("sensitive and instruction files cannot be granted")
		}
		exact[filepath.Clean(path)] = true
	}
	a.grant = fileGrant{
		thread: r.thread, workspace: r.workspace.Path(), paths: exact,
		remaining: 20, expires: a.now().Add(30 * time.Minute),
	}
	a.record(r, ApprovalRequest{}, "grant")
	return nil
}

func (a *Approvals) Revoke() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.grant = fileGrant{}
	if a.active != nil {
		a.active.permits = map[string]int{}
		a.record(a.active, ApprovalRequest{}, "revoke")
	}
}

func grantable(path string) bool {
	if workspace.Sensitive(path) {
		return false
	}
	switch strings.ToLower(filepath.Base(path)) {
	case "agents.md", "claude.md", "gemini.md":
		return false
	}
	return true
}

func (a *Approvals) useGrant(r *RunApproval, request ApprovalRequest) bool {
	if request.Tool != "edit" && request.Tool != "write" {
		return false
	}
	g := &a.grant
	sameSession := g.thread == r.thread && g.workspace == r.workspace.Path()
	available := g.remaining > 0 && a.now().Before(g.expires)
	pathAllowed := grantable(request.Path) && g.paths[filepath.Clean(request.Path)]
	if !sameSession || !available || !pathAllowed {
		return false
	}
	if err := r.workspace.Writable(request.Path); err != nil {
		return false
	}
	g.remaining--
	return true
}
