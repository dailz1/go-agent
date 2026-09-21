package app

import (
	"context"
	"time"

	"github.com/dailz1/go-agent/harness/internal/session"
	"github.com/dailz1/go-agent/harness/internal/snapshot"
)

// UIEvent is one host notification for the terminal layer: the render view
// plus the controller state it must not guess. It carries ordinary types
// only; no terminal-library type crosses this boundary.
type UIEvent struct {
	View      ViewModel
	Phase     SessionPhase
	State     RunState
	Session   session.Meta
	Open      bool
	SettleErr error
}

// GrantInfo is the read-only snapshot of the edit/write session grant.
// Mintable reports whether a run is active that could issue a new grant;
// grants bind to the active run's thread and cannot be minted while idle.
type GrantInfo struct {
	Active    bool
	Mintable  bool
	Usable    bool
	Paths     []string
	Remaining int
	Expires   time.Time
}

// Host is the terminal-facing control surface of the controller. The TUI
// renders UIEvent state and sends intents back; it never owns agent calls.
//
// Split by calling contract:
//
//   - Intents (NewSession … SetGrant, Changes, Restore) perform disk or
//     blocking work. They MUST be executed off the UI update loop by the
//     terminal bridge's intent runner, one at a time.
//   - Fast calls (Respond, GrantInfo, RevokeGrant, Observe registration)
//     only touch mutex-guarded memory or a one-slot buffered reply and are
//     safe to invoke directly from the update loop.
//
// Approval requests flow on their own channel and must never be dropped or
// coalesced: a lost request would hang a run until cancellation.
type Host interface {
	// Observe registers the event sink. It is called from the controller
	// worker; the bridge adapts it to a bounded, coalescing queue because
	// each event carries a full superseding snapshot.
	Observe(fn func(UIEvent))
	// ApprovalRequests exposes the run approval feed (single consumer).
	ApprovalRequests() <-chan ApprovalRequest
	// Idle is closed whenever no run worker, approval wait or settlement
	// is in flight; PhaseBlocked still gates the next action.
	Idle() <-chan struct{}

	// Intents.
	NewSession(ctx context.Context) (session.Meta, error)
	ListSessions() ([]session.Meta, error)
	OpenSession(ctx context.Context, id string) error
	Submit(ctx context.Context, input string) error
	ContinueRun(ctx context.Context) error
	AbandonRun(ctx context.Context) error
	RetrySettlement(ctx context.Context) error
	StopRun(ctx context.Context) error
	Close(ctx context.Context) error
	Changes(ctx context.Context) ([]snapshot.Record, error)
	Restore(ctx context.Context, changeID string) (snapshot.Record, error)
	SetGrant(paths []string) error

	// Fast calls.
	Respond(token string, id uint64, allow bool) bool
	GrantInfo() GrantInfo
	RevokeGrant()
}

// The terminal bridge consumes the controller only through Host.
var _ Host = (*Controller)(nil)

func (c *Controller) ApprovalRequests() <-chan ApprovalRequest {
	return c.startup.Approvals.Requests()
}

func (c *Controller) Respond(token string, id uint64, allow bool) bool {
	return c.startup.Approvals.Respond(token, id, allow)
}

func (c *Controller) GrantInfo() GrantInfo { return c.startup.Approvals.GrantInfo() }

func (c *Controller) RevokeGrant() { c.startup.Approvals.Revoke() }

// SetGrant mints a grant for the active run without exposing run tokens to
// the UI layer. Grant creation never answers a pending request.
func (c *Controller) SetGrant(paths []string) error { return c.startup.Approvals.GrantActive(paths) }

func (c *Controller) OpenSession(ctx context.Context, id string) error {
	_, err := c.Open(ctx, id)
	return err
}

func (c *Controller) ContinueRun(ctx context.Context) error     { return c.Continue(ctx) }
func (c *Controller) AbandonRun(ctx context.Context) error      { return c.Abandon(ctx) }
func (c *Controller) RetrySettlement(ctx context.Context) error { return c.RetrySettle(ctx) }
func (c *Controller) StopRun(ctx context.Context) error         { return c.Stop(ctx) }

// Changes lists the restore records of the open session for the /changes
// view. It requires idle state and opens the session snapshot store on
// demand, so it runs on the intent runner, never inside UI Update.
func (c *Controller) Changes(ctx context.Context) ([]snapshot.Record, error) {
	c.mu.Lock()
	if c.state != RunIdle {
		c.mu.Unlock()
		return nil, ErrRunActive
	}
	if c.open.ID == "" {
		c.mu.Unlock()
		return nil, ErrNoSession
	}
	aux, err := c.ensureAux(c.open.ID)
	var snapshots *snapshot.Store
	if err == nil {
		snapshots = aux.snapshots
	}
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return snapshots.List(ctx)
}

// Restore reverses one recorded change while no run is active. Conflict
// and verification failures come back as errors; the UI shows them instead
// of forcing an overwrite.
func (c *Controller) Restore(ctx context.Context, changeID string) (snapshot.Record, error) {
	c.mu.Lock()
	if c.state != RunIdle {
		c.mu.Unlock()
		return snapshot.Record{}, ErrRunActive
	}
	if c.open.ID == "" {
		c.mu.Unlock()
		return snapshot.Record{}, ErrNoSession
	}
	aux, err := c.ensureAux(c.open.ID)
	var snapshots *snapshot.Store
	if err == nil {
		snapshots = aux.snapshots
	}
	c.mu.Unlock()
	if err != nil {
		return snapshot.Record{}, err
	}
	return c.startup.Approvals.Restore(ctx, snapshots, changeID)
}
