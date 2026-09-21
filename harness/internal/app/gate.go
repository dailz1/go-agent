package app

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"

	"github.com/dailz1/go-agent/harness/internal/snapshot"
	"github.com/dailz1/go-agent/harness/internal/tools"
	"github.com/dailz1/go-agent/harness/internal/workspace"
	"github.com/dailz1/go-agent/tool"
)

// Gate joins the two Stage B halves for one run: the per-run approval credential
// and the session snapshot store. Commit approves the exact change, mints a
// fresh snapshot credential, then lets the store durably prepare before the
// single apply. Denial wraps tools.ErrDenied; snapshot persistence failures
// stay Go errors.
type Gate struct {
	run        *RunApproval
	snapshots  *snapshot.Store
	generation uint64
	sequence   atomic.Uint64
}

func (g *Gate) Commit(ctx context.Context, change workspace.Change, apply func() error) error {
	if err := g.run.approveChange(ctx, change); err != nil {
		return err
	}
	cred := snapshot.Credential{
		Workspace:    g.run.workspace.Path(),
		ThreadID:     g.run.thread,
		Generation:   g.generation,
		ToolSequence: g.sequence.Add(1),
	}
	_, err := g.snapshots.Commit(ctx, cred, change, apply)
	return err
}

// runRegistry wraps every tool so the in-tool approval checks share the exact
// one-use credential minted by the kernel approval callback, instead of asking
// twice. The delegate registry is what the agent consumes.
type runRegistry struct {
	delegate *tool.Registry
	run      *RunApproval
}

func (r runRegistry) Register(t tool.Tool) error {
	return r.delegate.Register(runTool{Tool: t, run: r.run})
}

// BeginRun assembles one run's registry: the five file tools plus shell, with
// approval modes, the snapshot gate and bounded output connected. The stores
// are session-scoped and must outlive the run; finish the run with Cancel and
// Finish, and never reuse a registry across runs.
func (s *Startup) BeginRun(
	ctx context.Context, thread string, snapshots *snapshot.Store, outputs *tools.OutputStore,
) (*RunApproval, *tool.Registry, error) {
	if snapshots == nil || outputs == nil {
		return nil, nil, errors.New("session snapshot and output stores are required before side effects")
	}
	run, err := s.Approvals.Begin(ctx, thread, s.Workspace)
	if err != nil {
		return nil, nil, err
	}
	generation, err := s.nextGeneration(ctx, snapshots)
	if err != nil {
		run.Cancel()
		return nil, nil, err
	}
	gate := &Gate{run: run, snapshots: snapshots, generation: generation}
	files := tools.New(s.Workspace, s.Rules, tools.Options{
		Writes:    gate,
		Sensitive: run,
		Outputs:   outputs,
		Excludes:  strings.Split(s.Config.Excludes, ","),
	})
	shell := tools.NewShell(s.Workspace, run, outputs, []string{s.Config.APIKeyEnv})
	registry := tool.NewRegistry()
	wrapped := runRegistry{delegate: registry, run: run}
	if err := files.Register(wrapped); err != nil {
		run.Cancel()
		return nil, nil, err
	}
	if err := wrapped.Register(shell); err != nil {
		run.Cancel()
		return nil, nil, err
	}
	return run, registry, nil
}

// nextGeneration keeps credentials unique across restarts: a clock-derived
// value, pushed above every generation already persisted for this session.
func (s *Startup) nextGeneration(ctx context.Context, snapshots *snapshot.Store) (uint64, error) {
	generation := uint64(s.Approvals.now().UnixNano())
	records, err := snapshots.List(ctx)
	if err != nil {
		return 0, err
	}
	for _, r := range records {
		if r.Credential.Generation >= generation {
			generation = r.Credential.Generation + 1
		}
	}
	return generation, nil
}

// Restore is the host command path for /restore: it reverses one recorded
// change while no run is active. The exclusive controller operation slot and
// the user confirmation dialog belong to the Stage D controller.
func (a *Approvals) Restore(ctx context.Context, s *snapshot.Store, id string) (snapshot.Record, error) {
	a.mu.Lock()
	active := a.active
	a.mu.Unlock()
	if active != nil {
		return snapshot.Record{}, errors.New("restore requires no active run")
	}
	return s.Restore(ctx, id)
}
