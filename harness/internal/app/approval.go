package app

import (
	"context"
	"crypto/rand"
	"errors"
	"sync"
	"time"

	"github.com/dailz1/go-agent/harness/internal/tools"
	"github.com/dailz1/go-agent/harness/internal/workspace"
)

// ApprovalRequest owns its strings and images; UI replies carry both identities.
type ApprovalRequest struct {
	Token     string
	ID        uint64
	ThreadID  string
	Workspace string
	Tool      string
	Arguments string
	Path      string
	Change    workspace.Change
	Command   string
	Cwd       string
	Timeout   time.Duration
}

type ApprovalAudit struct {
	Token  string
	ID     uint64
	Action string
	Tool   string
	Path   string
	At     time.Time
}

type pendingApproval struct {
	ctx   context.Context
	reply chan bool
}

// Approvals lives for the process, not a model response. Only the startup
// configuration may supply noApproval; it has no runtime setter.
type Approvals struct {
	mu         sync.Mutex
	noApproval bool
	closed     bool
	active     *RunApproval
	requests   chan ApprovalRequest
	next       uint64
	pending    map[uint64]pendingApproval
	grant      fileGrant
	audit      []ApprovalAudit
	now        func() time.Time
}

type RunApproval struct {
	manager   *Approvals
	ctx       context.Context
	cancel    context.CancelFunc
	token     string
	thread    string
	workspace *workspace.Workspace
	permits   map[string]int
}

func NewApprovals(noApproval bool) *Approvals {
	return &Approvals{
		noApproval: noApproval, requests: make(chan ApprovalRequest),
		pending: map[uint64]pendingApproval{}, audit: []ApprovalAudit{}, now: time.Now,
	}
}

func (a *Approvals) Requests() <-chan ApprovalRequest { return a.requests }

func (a *Approvals) Begin(ctx context.Context, thread string, w *workspace.Workspace) (*RunApproval, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || a.active != nil {
		return nil, errors.New("approval owner is closed or already running")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if thread == "" {
		return nil, errors.New("thread identity is required")
	}
	if a.grant.thread != thread || a.grant.workspace != w.Path() {
		a.grant = fileGrant{}
	}
	runCtx, cancel := context.WithCancel(ctx)
	run := &RunApproval{
		manager: a, ctx: runCtx, cancel: cancel, token: rand.Text(),
		thread: thread, workspace: w, permits: map[string]int{},
	}
	a.active = run
	return run, nil
}

func (r *RunApproval) Context() context.Context { return r.ctx }
func (r *RunApproval) Token() string            { return r.token }

// Cancel invalidates replies and grants before signaling cancellation. The
// controller still owns join/settlement and must call Finish only after join.
func (r *RunApproval) Cancel() {
	a := r.manager
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.active == r {
		a.pending = map[uint64]pendingApproval{}
		a.grant = fileGrant{}
		r.permits = map[string]int{}
		a.record(r, ApprovalRequest{}, "cancel")
	}
	r.cancel()
}

func (r *RunApproval) Finish() {
	a := r.manager
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.active == r {
		if r.ctx.Err() != nil {
			a.grant = fileGrant{}
		}
		a.active = nil
		a.pending = map[uint64]pendingApproval{}
		r.permits = map[string]int{}
	}
	r.cancel()
}

func (a *Approvals) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closed = true
	a.grant = fileGrant{}
	a.pending = map[uint64]pendingApproval{}
	if a.active != nil {
		a.active.cancel()
	}
}

func (a *Approvals) Respond(token string, id uint64, allow bool) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	pending, ok := a.pending[id]
	if !ok || a.active == nil || a.active.token != token {
		return false
	}
	if a.closed || a.active.ctx.Err() != nil || pending.ctx.Err() != nil {
		return false
	}
	delete(a.pending, id)
	pending.reply <- allow
	return true
}

func (r *RunApproval) check(ctx context.Context) error {
	if err := r.ctx.Err(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.manager.closed || r.manager.active != r {
		return tools.ErrDenied
	}
	return nil
}

func (r *RunApproval) authorize(ctx context.Context, request ApprovalRequest) error {
	a := r.manager
	a.mu.Lock()
	if err := r.check(ctx); err != nil {
		a.mu.Unlock()
		return err
	}
	a.next++
	request.Token, request.ID = r.token, a.next
	request.ThreadID, request.Workspace = r.thread, r.workspace.Path()
	if a.noApproval || a.useGrant(r, request) {
		a.record(r, request, "automatic")
		a.mu.Unlock()
		return nil
	}
	reply := make(chan bool, 1)
	a.pending[request.ID] = pendingApproval{ctx: ctx, reply: reply}
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.pending, request.ID)
		a.mu.Unlock()
	}()
	select {
	case a.requests <- request:
	case <-r.ctx.Done():
		return r.ctx.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
	var allow bool
	select {
	case allow = <-reply:
	case <-r.ctx.Done():
		return r.ctx.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := r.check(ctx); err != nil {
		return err
	}
	if !allow {
		a.record(r, request, "denied")
		return tools.ErrDenied
	}
	a.record(r, request, "approved")
	return nil
}

func (a *Approvals) record(r *RunApproval, request ApprovalRequest, action string) {
	a.audit = append(a.audit, ApprovalAudit{
		Token: r.token, ID: request.ID, Action: action,
		Tool: request.Tool, Path: request.Path, At: a.now(),
	})
}

// Audit returns host metadata for the session controller to persist.
func (a *Approvals) Audit() []ApprovalAudit {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]ApprovalAudit{}, a.audit...)
}
