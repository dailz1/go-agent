package app

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/dailz1/go-agent/agent"
	"github.com/dailz1/go-agent/harness/internal/session"
	"github.com/dailz1/go-agent/harness/internal/snapshot"
	"github.com/dailz1/go-agent/harness/internal/tools"
	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

// Controller errors. They gate user actions; the phase accessor explains
// which rule fired.
var (
	ErrRunActive         = errors.New("a run is active; stop it first")
	ErrDecisionRequired  = errors.New("session has an interrupted run: continue it or abandon it first")
	ErrSettlementPending = errors.New("settlement is unfinished; retry it before new input")
	ErrNoSession         = errors.New("no session is open")
)

// SessionPhase is the durable decision state of the open session. Phases
// Fresh, Complete and Settled accept new input; Incomplete demands an
// explicit continue-or-abandon decision; Blocked holds a failed settlement.
type SessionPhase int

const (
	PhaseFresh SessionPhase = iota
	PhaseComplete
	PhaseSettled
	PhaseIncomplete
	PhaseBlocked
)

func (p SessionPhase) String() string {
	switch p {
	case PhaseFresh:
		return "fresh"
	case PhaseComplete:
		return "complete"
	case PhaseSettled:
		return "settled"
	case PhaseIncomplete:
		return "interrupted"
	case PhaseBlocked:
		return "settlement-pending"
	}
	return "unknown"
}

// RunState is the controller execution state.
type RunState int

const (
	RunIdle RunState = iota
	RunRunning
	RunStopping
)

type runOutcome int

const (
	outcomePending runOutcome = iota
	outcomeDone
	outcomeInterrupted
)

type activeRun struct {
	thread   string
	ctx      context.Context
	cancel   context.CancelFunc
	approval *RunApproval
	done     chan struct{}

	// token is the run-bound settlement credential, captured by the kernel
	// exit callback after owned work exits and before ownership release.
	token     agent.SettlementToken
	tokenOK   bool
	outcome   runOutcome
	finalView ViewModel
}

// auxStores are the session-scoped snapshot and output stores. They outlive
// single runs and close when the session is switched away or the controller
// closes.
type auxStores struct {
	snapshots *snapshot.Store
	outputs   *tools.OutputStore
}

// Controller owns the durable session lifecycle: one open session, one
// active run, cancel-join-settle with the original token, and the continue
// versus redirect decision after an interruption. The kernel is consumed
// only through its public persistence APIs.
type Controller struct {
	startup       *Startup
	mgr           *session.Manager
	store         store.Store
	controlAgent  *agent.Agent // read-only/settlement surface; never executes
	settleTimeout time.Duration
	runTimeout    time.Duration
	observe       func(ViewModel)

	mu        sync.Mutex
	open      session.Meta
	phase     SessionPhase
	state     RunState
	run       *activeRun
	view      ViewModel
	settleErr error
	aux       *auxStores
	idleCh    chan struct{} // closed whenever the controller returns to idle
}

// NewController wires the session manager into the startup resources. No
// model call and no session data is created until NewSession or Open.
func NewController(startup *Startup, mgr *session.Manager) *Controller {
	c := &Controller{
		startup: startup, mgr: mgr, store: mgr.Store(),
		settleTimeout: 10 * time.Second,
	}
	c.idleCh = make(chan struct{})
	close(c.idleCh)
	if d, err := time.ParseDuration(startup.Config.RunTimeout); err == nil && d > 0 {
		c.runTimeout = d
	}
	c.controlAgent = agent.New(startup.Provider, tool.NewRegistry(), agent.WithStore(c.store))
	return c
}

// SettleTimeout overrides the bounded settlement context (tests).
func (c *Controller) SettleTimeout(d time.Duration) { c.settleTimeout = d }

// Observe registers the synchronous view sink. It is called from the worker
// goroutine; stage E adapts it to a bounded UI queue.
func (c *Controller) Observe(fn func(ViewModel)) {
	c.mu.Lock()
	c.observe = fn
	c.mu.Unlock()
}

func (c *Controller) notify(view ViewModel) {
	c.mu.Lock()
	fn := c.observe
	c.mu.Unlock()
	if fn != nil {
		fn(view)
	}
}

// Phase reports the open session's decision state.
func (c *Controller) Phase() SessionPhase {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.phase
}

// State reports the controller execution state.
func (c *Controller) State() RunState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

// Idle returns a channel closed whenever the controller has no active
// worker: the run worker, approvals and (when stopping) settlement all
// finished. A settlement blocked on retry also closes it — PhaseBlocked
// gates the next action.
func (c *Controller) Idle() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.idleCh
}

func (c *Controller) markRunningLocked() {
	c.idleCh = make(chan struct{})
	c.state = RunRunning
}

func (c *Controller) markIdleLocked() {
	if c.state != RunIdle {
		c.state = RunIdle
	}
	c.run = nil
	c.closeIdleLocked()
}

func (c *Controller) closeIdleLocked() {
	select {
	case <-c.idleCh:
	default:
		close(c.idleCh)
	}
}

// View returns the current view model snapshot.
func (c *Controller) View() ViewModel {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.view
}

// SettleErr reports the failure that blocked settlement, if any.
func (c *Controller) SettleErr() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.settleErr
}

// Session returns the open session metadata.
func (c *Controller) Session() (session.Meta, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.open.ID == "" {
		return session.Meta{}, ErrNoSession
	}
	return c.open, nil
}

// NewSession persists a fresh session identity before anything can execute
// against it and makes it the open session.
func (c *Controller) NewSession(ctx context.Context) (session.Meta, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != RunIdle {
		return session.Meta{}, ErrRunActive
	}
	meta, err := c.mgr.Create(c.startup.Workspace.Path(), c.startup.Config.Provider, c.startup.Config.Model)
	if err != nil {
		return session.Meta{}, err
	}
	c.closeAuxLocked()
	c.open = meta
	c.phase = PhaseFresh
	c.settleErr = nil
	c.view = FromCache(session.Cache{}, false, 0, meta.ID)
	return meta, nil
}

// ListSessions lists persisted sessions; it never touches the provider.
func (c *Controller) ListSessions() ([]session.Meta, error) { return c.mgr.List() }

// OpenResult is the opened-session contract for the UI: metadata, the
// decision phase, and the cache-graded view. Opening never executes or
// probes the model.
type OpenResult struct {
	Meta  session.Meta
	Phase SessionPhase
	View  ViewModel
}

// Open makes an existing session current. The incomplete check is the
// read-only SettlementTarget; the view is the display cache graded against
// the store head. No model call happens here.
func (c *Controller) Open(ctx context.Context, id string) (OpenResult, error) {
	c.mu.Lock()
	if c.state != RunIdle {
		c.mu.Unlock()
		return OpenResult{}, ErrRunActive
	}
	c.mu.Unlock()

	meta, ok, err := c.mgr.Get(id)
	if err != nil {
		return OpenResult{}, err
	}
	if !ok {
		return OpenResult{}, fmt.Errorf("session %q does not exist", id)
	}
	head, err := c.storeHead(ctx, id)
	if err != nil {
		return OpenResult{}, err
	}
	phase, err := c.derivePhase(ctx, id, head, meta.LastKnown)
	if err != nil {
		return OpenResult{}, err
	}
	cache, cacheOK, err := c.mgr.LoadCache(id)
	if err != nil {
		return OpenResult{}, err
	}
	view := FromCache(cache, cacheOK, head, id)

	c.mu.Lock()
	c.closeAuxLocked()
	c.open = meta
	c.phase = phase
	c.settleErr = nil
	c.view = view
	c.mu.Unlock()
	return OpenResult{Meta: meta, Phase: phase, View: view}, nil
}

// derivePhase reads the durable state read-only. An active run demands an
// explicit decision; a terminal log accepts input; an empty log is fresh.
func (c *Controller) derivePhase(ctx context.Context, id string, head int64, lastKnown session.Status) (SessionPhase, error) {
	_, err := c.controlAgent.SettlementTarget(ctx, id)
	switch {
	case err == nil:
		return PhaseIncomplete, nil
	case errors.Is(err, agent.ErrNothingToSettle):
		if head == 0 {
			return PhaseFresh, nil
		}
		if lastKnown == session.StatusCancelled {
			return PhaseSettled, nil
		}
		return PhaseComplete, nil
	default:
		return 0, err
	}
}

// Submit starts a new run on the open session with a fresh input. On a
// settled or completed thread this is the same-thread redirect: the kernel
// replays the durable history and calls the model only for the new input.
func (c *Controller) Submit(ctx context.Context, input string) error {
	c.mu.Lock()
	if c.state != RunIdle {
		c.mu.Unlock()
		return ErrRunActive
	}
	switch c.phase {
	case PhaseIncomplete:
		c.mu.Unlock()
		return ErrDecisionRequired
	case PhaseBlocked:
		c.mu.Unlock()
		return ErrSettlementPending
	case PhaseFresh, PhaseComplete, PhaseSettled:
	default:
		c.mu.Unlock()
		return ErrNoSession
	}
	meta := c.open
	if meta.ID == "" {
		c.mu.Unlock()
		return ErrNoSession
	}
	// The input commit is preceded by durable metadata: the first input
	// fixes the title and the running status before any model call.
	if meta.Title == "" {
		meta.Title = session.Title(input)
	}
	meta.LastKnown = session.StatusRunning
	if err := c.mgr.Update(meta); err != nil {
		c.mu.Unlock()
		return fmt.Errorf("persist session before run: %w", err)
	}
	c.open = meta
	aux, err := c.ensureAux(meta.ID)
	if err != nil {
		c.mu.Unlock()
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	if c.runTimeout > 0 {
		runCtx, cancel = context.WithTimeout(runCtx, c.runTimeout)
	}
	run := c.activeRunLocked(runCtx, cancel, meta.ID)
	c.markRunningLocked()
	c.mu.Unlock()

	go c.streamWorker(run, meta, aux, input)
	return nil
}

// Continue resumes the interrupted run of the open session: non-streaming
// ResumeThread per D3, explicitly chosen by the user, without appending a
// new input.
func (c *Controller) Continue(ctx context.Context) error {
	c.mu.Lock()
	if c.state != RunIdle {
		c.mu.Unlock()
		return ErrRunActive
	}
	if c.phase != PhaseIncomplete {
		c.mu.Unlock()
		return fmt.Errorf("continue requires an interrupted run, phase is %s", c.phase)
	}
	meta := c.open
	meta.LastKnown = session.StatusRunning
	if err := c.mgr.Update(meta); err != nil {
		c.mu.Unlock()
		return err
	}
	c.open = meta
	aux, err := c.ensureAux(meta.ID)
	if err != nil {
		c.mu.Unlock()
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	if c.runTimeout > 0 {
		runCtx, cancel = context.WithTimeout(runCtx, c.runTimeout)
	}
	run := c.activeRunLocked(runCtx, cancel, meta.ID)
	c.markRunningLocked()
	c.mu.Unlock()

	go c.resumeWorker(run, meta, aux)
	return nil
}

// Abandon is the explicit give-up decision on an interrupted run: a fresh
// SettlementTarget is taken now and settled, so the session accepts a new
// input (redirect) afterwards.
func (c *Controller) Abandon(ctx context.Context) error {
	c.mu.Lock()
	if c.state != RunIdle {
		c.mu.Unlock()
		return ErrRunActive
	}
	if c.phase == PhaseBlocked {
		c.mu.Unlock()
		return ErrSettlementPending
	}
	id := c.open.ID
	c.mu.Unlock()
	if id == "" {
		return ErrNoSession
	}
	return c.settleInterrupted(ctx, id)
}

// RetrySettle retries a blocked settlement with the original token. Only a
// successful settlement (or a nothing-to-settle race) unblocks new input.
func (c *Controller) RetrySettle(ctx context.Context) error {
	c.mu.Lock()
	if c.state != RunIdle {
		c.mu.Unlock()
		return ErrRunActive
	}
	if c.phase != PhaseBlocked {
		c.mu.Unlock()
		return fmt.Errorf("no settlement is pending, phase is %s", c.phase)
	}
	token := c.run.token
	id := c.open.ID
	c.mu.Unlock()

	err := c.settle(ctx, token)
	c.mu.Lock()
	c.applySettleResultLocked(id, err, token)
	c.mu.Unlock()
	c.notify(c.View())
	return c.settleErrForPhase(err)
}

// Stop is the user stop path: invalidate approvals and grants, cancel, join
// the run, then settle with the ORIGINAL run-bound token captured before
// ownership was released. Settlement uses a fresh bounded context.
func (c *Controller) Stop(ctx context.Context) error {
	c.mu.Lock()
	for {
		if c.state == RunStopping {
			run := c.run
			c.mu.Unlock()
			<-run.done
			c.mu.Lock()
			continue
		}
		if c.state != RunRunning {
			c.mu.Unlock()
			return ErrRunActive
		}
		break
	}
	run := c.run
	c.state = RunStopping
	meta := c.open
	c.mu.Unlock()

	run.approval.Cancel() // invalidate pending replies and grants first
	run.cancel()          // then signal cancellation to the run
	<-run.done            // join: iterator and owned work exited

	// The completion race: the run finished naturally before cancellation
	// landed. The completion stands; nothing is settled.
	if !run.tokenOK {
		c.finishStoppedRun(meta, run, nil)
		c.notify(c.View())
		return nil
	}
	err := c.settle(ctx, run.token)
	c.mu.Lock()
	if errors.Is(err, agent.ErrNothingToSettle) {
		// The run reached a durable terminal state without this settle:
		// natural completion won the race and stands. Anything else is
		// re-derived from the log instead of guessed.
		if run.outcome == outcomeDone {
			c.finishRunLocked(meta, run, session.StatusComplete, PhaseComplete)
		} else {
			phase := c.reevaluatePhaseLocked(context.Background(), meta.ID)
			status := session.StatusCancelled
			if phase == PhaseComplete {
				status = session.StatusComplete
			}
			c.finishRunLocked(meta, run, status, phase)
		}
		c.mu.Unlock()
		return nil
	}
	c.applySettleResultLocked(meta.ID, err, run.token)
	if err == nil {
		c.finishRunLocked(meta, run, session.StatusCancelled, PhaseSettled)
	} else {
		// Blocked: the worker has exited; only the settlement retry is
		// pending, gated by PhaseBlocked. The run stays for its token.
		c.state = RunIdle
		c.closeIdleLocked()
	}
	c.mu.Unlock()
	c.notify(c.View())
	return c.settleErrForPhase(err)
}

// Close is the EOF path: stop any active run through the settle protocol,
// release session stores and the thread store.
func (c *Controller) Close(ctx context.Context) error {
	var stopErr error
	if c.State() == RunRunning {
		stopErr = c.Stop(ctx)
	}
	c.mu.Lock()
	c.closeAuxLocked()
	c.mu.Unlock()
	if err := c.mgr.Close(); err != nil {
		if stopErr == nil {
			stopErr = err
		}
	}
	return stopErr
}

// settleInterrupted takes the fresh recovery decision and settles it.
func (c *Controller) settleInterrupted(ctx context.Context, id string) error {
	target, err := c.controlAgent.SettlementTarget(ctx, id)
	switch {
	case errors.Is(err, agent.ErrNothingToSettle):
		// Already terminal (e.g. settled by another decision): accept input.
		c.mu.Lock()
		c.refreshTerminalLocked(ctx, id)
		c.mu.Unlock()
		return nil
	case err != nil:
		return err
	}
	serr := c.settle(ctx, *target)
	c.mu.Lock()
	c.applySettleResultLocked(id, serr, *target)
	c.mu.Unlock()
	c.notify(c.View())
	return c.settleErrForPhase(serr)
}

// settle invokes SettleThread under a fresh bounded context. The callback
// contract forbids reenter the thread from the exit hook; settling happens
// strictly after the join here.
func (c *Controller) settle(ctx context.Context, token agent.SettlementToken) error {
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.settleTimeout)
	defer cancel()
	return c.controlAgent.SettleThread(settleCtx, token)
}

func (c *Controller) settleErrForPhase(err error) error {
	if err != nil && !errors.Is(err, agent.ErrNothingToSettle) {
		return fmt.Errorf("settle thread: %w", err)
	}
	return nil
}

// applySettleResultLocked records the settlement outcome. Success flips the
// session to settled and refreshes the display cache from the guaranteed
// read-only cancelled snapshot; failure blocks new input until an explicit
// retry of the SAME token.
func (c *Controller) applySettleResultLocked(id string, err error, token agent.SettlementToken) {
	if err == nil {
		c.settleErr = nil
		c.phase = PhaseSettled
		c.refreshCacheAfterSettle(context.Background(), id)
		c.persistStatusLocked(session.StatusCancelled)
		c.markIdleLocked()
		return
	}
	if errors.Is(err, agent.ErrNothingToSettle) {
		// Nothing was incomplete after all: treat as terminal and re-derive.
		c.settleErr = nil
		c.refreshTerminalLocked(context.Background(), id)
		return
	}
	c.settleErr = err
	c.phase = PhaseBlocked
	// Keep the original token for the retry; never refresh it.
	if c.run != nil {
		c.run.token = token
		c.run.tokenOK = true
	}
}

func (c *Controller) activeRunLocked(ctx context.Context, cancel context.CancelFunc, thread string) *activeRun {
	run := &activeRun{thread: thread, ctx: ctx, cancel: cancel, done: make(chan struct{})}
	c.run = run
	return run
}

// ensureAux opens the session-scoped stores once per session. Caller holds
// c.mu.
func (c *Controller) ensureAux(id string) (*auxStores, error) {
	if c.aux != nil && c.aux.snapshots != nil {
		return c.aux, nil
	}
	snapshots, err := snapshot.Open(c.mgr.SnapshotDir(id), c.startup.Workspace, id,
		int64(c.startup.Config.SnapshotQuotaMiB)<<20)
	if err != nil {
		return nil, fmt.Errorf("open snapshot store: %w", err)
	}
	outputs, err := tools.OpenOutputStore(c.mgr.OutputDir(id))
	if err != nil {
		snapshots.Close()
		return nil, fmt.Errorf("open output store: %w", err)
	}
	c.aux = &auxStores{snapshots: snapshots, outputs: outputs}
	return c.aux, nil
}

func (c *Controller) closeAuxLocked() {
	if c.aux != nil {
		if c.aux.snapshots != nil {
			c.aux.snapshots.Close()
		}
		if c.aux.outputs != nil {
			c.aux.outputs.Close()
		}
		c.aux = nil
	}
}

// buildRunAgent assembles one run's kernel agent: per-run registry and
// approval credential, the frozen system prompt (captured by the kernel only
// at thread creation), and the exit hook that captures the settlement token.
func (c *Controller) buildRunAgent(run *activeRun, registry *tool.Registry) *agent.Agent {
	cfg := c.startup.Config
	opts := []agent.Option{
		agent.WithStore(c.store),
		agent.WithApprovalFn(run.approval.ApprovalFn),
		agent.WithRunExitFn(func(token agent.SettlementToken) {
			c.mu.Lock()
			run.token = token
			run.tokenOK = true
			c.mu.Unlock()
		}),
		agent.WithSystemPrompt(c.startup.Rules.Snapshot().System),
		agent.WithLLMOptions(llm.WithModel(cfg.Model), llm.WithMaxTokens(cfg.MaxOutputTokens)),
		agent.WithContextWindowTokens(cfg.ContextBudget),
		agent.WithMaxIter(cfg.MaxIterations),
	}
	return agent.New(c.startup.Provider, registry, opts...)
}

// beginRun assembles the approval credential and tool registry for one run.
func (c *Controller) beginRun(ctx context.Context, thread string, aux *auxStores) (*RunApproval, *tool.Registry, error) {
	return c.startup.BeginRun(ctx, thread, aux.snapshots, aux.outputs)
}

// streamWorker consumes one RunThreadStream on the worker goroutine: the
// only consumer of the event stream. Done and compaction snapshots are
// captured (head included) before the view is published.
func (c *Controller) streamWorker(run *activeRun, meta session.Meta, aux *auxStores, input string) {
	reducer := NewReducer(meta.ID)
	approval, registry, err := c.beginRun(run.ctx, meta.ID, aux)
	if err != nil {
		run.outcome = outcomeInterrupted
		c.mu.Lock()
		c.finishRunLocked(meta, run, session.StatusBroken, PhaseIncomplete)
		c.mu.Unlock()
		close(run.done)
		return
	}
	run.approval = approval
	agentRun := c.buildRunAgent(run, registry)

	seq, err := agentRun.RunThreadStream(run.ctx, meta.ID, input)
	if err != nil {
		run.outcome = outcomeInterrupted
		c.workerEnded(run, meta, approval)
		return
	}
	var streamErr error
	for ev, err := range seq {
		if err != nil {
			streamErr = err
			break
		}
		switch ev.(type) {
		case agent.DoneEvent, *agent.DoneEvent, agent.CompactionEvent, *agent.CompactionEvent:
			c.captureCache(run.ctx, meta.ID, ev)
		}
		reducer.Apply(ev)
		if _, done := ev.(agent.DoneEvent); done {
			run.outcome = outcomeDone
		} else if _, done := ev.(*agent.DoneEvent); done {
			run.outcome = outcomeDone
		}
		c.mu.Lock()
		c.view = reducer.View()
		view := c.view
		c.mu.Unlock()
		c.notify(view)
	}
	if streamErr != nil {
		run.outcome = outcomeInterrupted
	}
	c.workerEnded(run, meta, approval)
}

// resumeWorker executes the explicit continue decision: non-streaming
// ResumeThread, cancellable, with real tool and approval activity but no
// fabricated stream deltas (D3).
func (c *Controller) resumeWorker(run *activeRun, meta session.Meta, aux *auxStores) {
	reducer := NewReducer(meta.ID)
	reducer.SetResuming(true)
	c.mu.Lock()
	c.view = reducer.View()
	startView := c.view
	c.mu.Unlock()
	c.notify(startView)

	approval, registry, err := c.beginRun(run.ctx, meta.ID, aux)
	if err != nil {
		run.outcome = outcomeInterrupted
		c.mu.Lock()
		c.finishRunLocked(meta, run, session.StatusBroken, PhaseIncomplete)
		c.mu.Unlock()
		close(run.done)
		return
	}
	run.approval = approval
	agentRun := c.buildRunAgent(run, registry)

	result, err := agentRun.ResumeThread(run.ctx, meta.ID)
	reducer.SetResuming(false)
	switch {
	case err != nil:
		run.outcome = outcomeInterrupted
	case result != nil && result.Cancelled:
		// The thread was settled elsewhere; the snapshot is read-only truth.
		run.outcome = outcomeDone
		c.mu.Lock()
		c.view = reducer.View()
		c.mu.Unlock()
		c.workerEndedCancelled(run, meta, approval)
		return
	default:
		run.outcome = outcomeDone
		reducer.CompleteWith(result)
		c.mu.Lock()
		c.view = reducer.View()
		if head, herr := c.storeHead(run.ctx, meta.ID); herr == nil {
			c.saveCache(meta.ID, session.Cache{Head: head, History: result.History})
			updated := c.view
			updated.Head = head
			c.view = updated
		}
		c.mu.Unlock()
	}
	c.workerEnded(run, meta, approval)
}

// workerEnded performs the natural-end bookkeeping unless Stop owns the
// terminal transition, then joins the approval slot and signals completion.
func (c *Controller) workerEnded(run *activeRun, meta session.Meta, approval *RunApproval) {
	c.mu.Lock()
	if c.state != RunStopping {
		switch run.outcome {
		case outcomeDone:
			c.finishRunLocked(meta, run, session.StatusComplete, PhaseComplete)
		default:
			// Recoverable interruption (provider/disk error, run timeout):
			// no auto-abandon; the user decides continue versus redirect.
			c.finishRunLocked(meta, run, session.StatusBroken, PhaseIncomplete)
		}
	}
	c.mu.Unlock()
	approval.Finish()
	close(run.done)
	c.notify(run.finalView)
}

func (c *Controller) workerEndedCancelled(run *activeRun, meta session.Meta, approval *RunApproval) {
	c.mu.Lock()
	if c.state != RunStopping {
		c.finishRunLocked(meta, run, session.StatusCancelled, PhaseSettled)
	}
	c.mu.Unlock()
	approval.Finish()
	close(run.done)
	c.notify(run.finalView)
}

// finishStoppedRun completes a stop whose run never established a session
// (no token): the durable state decides the phase.
func (c *Controller) finishStoppedRun(meta session.Meta, run *activeRun, _ error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	phase := c.reevaluatePhaseLocked(context.Background(), meta.ID)
	status := session.StatusCancelled
	if phase == PhaseComplete {
		status = session.StatusComplete
	}
	c.finishRunLocked(meta, run, status, phase)
}

// finishRunLocked records the terminal phase for a naturally ended run.
func (c *Controller) finishRunLocked(meta session.Meta, run *activeRun, status session.Status, phase SessionPhase) {
	meta.LastKnown = status
	if err := c.mgr.Update(meta); err != nil {
		// Metadata is a display hint; the store log stays authoritative.
		_ = err
	}
	c.open = meta
	c.phase = phase
	c.markIdleLocked()
	final := c.view
	if run.outcome == outcomeInterrupted && phase != PhaseSettled {
		final.Interrupted = true
	}
	run.finalView = final
}

func (c *Controller) reevaluatePhaseLocked(ctx context.Context, id string) SessionPhase {
	head, err := c.storeHead(ctx, id)
	if err != nil {
		return PhaseIncomplete
	}
	phase, err := c.derivePhase(ctx, id, head, c.open.LastKnown)
	if err != nil {
		return PhaseIncomplete
	}
	return phase
}

func (c *Controller) refreshTerminalLocked(ctx context.Context, id string) {
	phase := c.reevaluatePhaseLocked(ctx, id)
	c.phase = phase
	c.settleErr = nil
}

// persistStatusLocked records a last-known status without touching phases.
func (c *Controller) persistStatusLocked(status session.Status) {
	meta := c.open
	meta.LastKnown = status
	if err := c.mgr.Update(meta); err != nil {
		_ = err // display hint only; the log is authoritative
	}
	c.open = meta
}

// captureCache durably saves the authoritative history with the store head
// it was consistent with, before the view is published.
func (c *Controller) captureCache(ctx context.Context, id string, ev agent.AgentEvent) {
	var history []llm.Message
	switch e := ev.(type) {
	case agent.DoneEvent:
		history = e.History
	case *agent.DoneEvent:
		history = e.History
	case agent.CompactionEvent:
		history = e.History
	case *agent.CompactionEvent:
		history = e.History
	default:
		return
	}
	head, err := c.storeHead(ctx, id)
	if err != nil {
		return // honest gap: next open reports the trailing cache
	}
	c.mu.Lock()
	c.saveCache(id, session.Cache{Head: head, History: history})
	view := c.view
	view.Head = head
	c.view = view
	c.mu.Unlock()
}

func (c *Controller) saveCache(id string, cache session.Cache) {
	if err := c.mgr.SaveCache(id, cache); err != nil {
		// A failed save is a display gap, never a durability claim.
		_ = err
	}
}

// refreshCacheAfterSettle uses the guaranteed read-only cancelled snapshot
// of a settled thread to refresh the display cache. No model call happens.
func (c *Controller) refreshCacheAfterSettle(ctx context.Context, id string) {
	res, err := c.controlAgent.ResumeThread(ctx, id)
	if err != nil || res == nil || !res.Cancelled {
		return // best effort; the interrupted live view stays honest
	}
	head, err := c.storeHead(ctx, id)
	if err != nil {
		return
	}
	c.saveCache(id, session.Cache{Head: head, History: res.History})
	view := c.view
	view.History = append([]llm.Message(nil), res.History...)
	view.Head = head
	view.Cache = CacheCurrent
	c.view = view
}

func (c *Controller) storeHead(ctx context.Context, id string) (int64, error) {
	state, err := c.store.Latest(ctx, id)
	if err != nil {
		return 0, fmt.Errorf("read thread head: %w", err)
	}
	return state.Head, nil
}
