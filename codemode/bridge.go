package codemode

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dailz1/go-agent/tool"
)

// nestedExecKey is the private context marker carried by every nested tool
// call (D7). It is unexported: host code cannot forge it, only forward it.
type nestedExecKey struct{}

// hardAgentError wraps a hard fault for the Agent loop with a bounded,
// non-blocking Error(): the category text only. The cause stays reachable
// via Unwrap for diagnostics that choose to format it (D12/F15).
type hardAgentError struct {
	category string
	cause    error
}

func (e *hardAgentError) Error() string { return "codemode: " + e.category }
func (e *hardAgentError) Unwrap() error { return e.cause }

// execResponse is what supervise hands back to runCodeTool.Execute.
type execResponse struct {
	result            *tool.ToolResult
	hardErr           error
	cleanupIncomplete bool
}

// runResult is the buffered owner-done signal.
type runResult struct {
	outcome RunOutcome
	err     error
}

// hardFault is a D12 hard error: it selects the terminal cause when none is
// registered and interrupts the Agent round via a wrapped Go error.
type hardFault struct {
	err     error
	message string
}

type completion struct {
	callID  uint64
	value   json.RawMessage
	failure *Failure
}

type hostJob struct {
	callID     uint64
	name       string
	args       json.RawMessage
	acceptedAt time.Time
}

// execution is one admitted run_code run and everything it owns: admission
// and delivery gates, counters, host workers, the inbox, the image table
// and the stdout budget. e.mu is the short critical section shared by the
// stop transition and the delivery reservation (C/D13); it is never held
// across user code, tool calls, approval callbacks or resolver entries.
type execution struct {
	id       string
	gen      *generation
	mode     *CodeMode
	executor Executor
	source   *tool.Registry
	limits   Limits
	ctx      context.Context
	cancel   context.CancelFunc

	mu                 sync.Mutex
	admOpen            bool
	deliveryOpen       bool
	stopReq            bool
	settled            bool
	causeSet           bool
	cause              FailureKind
	causeMsg           string
	callsSubmitted     int
	pendingCalls       int
	deliveriesInFlight int
	hostOutstanding    int
	vmExited           bool
	cleanupComplete    bool
	responseReturned   bool
	interruptFn        func()
	outcome            *RunOutcome
	callSeq            uint64
	hostDataBytes      int
	imagesAccepted     int
	wall               *time.Timer

	jobs        chan hostJob
	inbox       chan completion
	stopCh      chan struct{}
	drainCh     chan struct{}
	drainOnce   sync.Once
	workersWG   sync.WaitGroup
	ownerWG     sync.WaitGroup
	ownerDone   chan runResult
	hardErrCh   chan hardFault
	hardPending atomic.Bool

	imagesMu sync.Mutex
	images   map[string]tool.Image
	imageSeq int

	stdoutMu   sync.Mutex
	stdoutBuf  []byte
	stdoutFull bool
}

// newExecution builds the record and starts its goroutines: one VM owner
// (Executor.Run) and a bounded worker pool. Admission counting happened in
// Execute under the CodeMode ledger lock.
func (m *CodeMode) newExecution(t *runCodeTool, parent context.Context, code string, wall *time.Timer) *execution {
	// The nested-execution marker rides the call ctx: any wrapper that
	// forwards ctx into another run_code Execute is rejected (D7/F14).
	ctx, cancel := context.WithCancel(context.WithValue(parent, nestedExecKey{}, struct{}{}))
	e := &execution{
		id:        fmt.Sprintf("exec-%d", m.seq.Add(1)),
		gen:       t.gen,
		mode:      m,
		executor:  m.executor,
		source:    m.source,
		ctx:       ctx,
		cancel:    cancel,
		wall:      wall,
		jobs:      make(chan hostJob, m.cfg.maxCalls),
		inbox:     make(chan completion, m.cfg.maxCalls),
		stopCh:    make(chan struct{}),
		drainCh:   make(chan struct{}),
		ownerDone: make(chan runResult, 1),
		hardErrCh: make(chan hardFault, 1),
		images:    make(map[string]tool.Image),
	}
	e.limits = Limits{
		WallClock:          m.cfg.wallClock,
		VMExitTimeout:      m.cfg.vmExitTimeout,
		HostDrainTimeout:   m.cfg.hostDrainTimeout,
		MaxCalls:           m.cfg.maxCalls,
		MaxHostConcurrency: m.cfg.maxHostConcurrency,
		SourceBytes:        m.cfg.sourceBytes,
		ArgsBytes:          m.cfg.argsBytes,
		HostResultBytes:    m.cfg.hostResultBytes,
		HostDataBytes:      m.cfg.hostDataBytes,
		MaxImages:          m.cfg.maxImages,
		StdoutBytes:        m.cfg.stdoutBytes,
		OutputBytes:        m.cfg.outputBytes,
		ProjectionDepth:    m.cfg.projectionDepth,
		ProjectionElements: m.cfg.projectionElements,
		GuestSteps:         m.cfg.guestSteps,
		GuestMemoryBytes:   m.cfg.guestMemoryBytes,
	}
	e.mu.Lock()
	e.admOpen = true
	e.deliveryOpen = true
	e.mu.Unlock()

	e.workersWG.Add(m.cfg.maxHostConcurrency)
	e.ownerWG.Add(1)
	go func() {
		defer e.ownerWG.Done()
		// A panic from any Executor implementation (custom backends included)
		// must not escape the owner goroutine: the Agent's Tool.Execute
		// recovery cannot catch it across this boundary. Route it through the
		// normal completion path as an infrastructure fault (ocr high).
		var outcome RunOutcome
		var runErr error
		defer func() {
			if recovered := recover(); recovered != nil {
				outcome = RunOutcome{}
				runErr = fmt.Errorf("executor panicked: %v", recovered)
			}
			e.ownerDone <- runResult{outcome: outcome, err: runErr}
		}()
		req := RunRequest{
			Source:     code,
			Generation: t.gen.version,
			Tools:      cloneDescriptors(t.gen.tools),
			Host:       hostAdapter{e},
			Limits:     e.limits,
			Control:    &executionControl{e},
		}
		outcome, runErr = m.executor.Run(ctx, req)
	}()
	for range m.cfg.maxHostConcurrency {
		go func() {
			defer e.workersWG.Done()
			e.workerLoop()
		}()
	}
	return e
}

// hostAdapter adapts the execution to the executor-facing Host interface.
type hostAdapter struct{ e *execution }

// cloneDescriptors deep-copies the descriptor slice including schema bytes:
// a custom Executor must not be able to reach or mutate the generation's
// canonical bytes through RunRequest (ocr medium).
func cloneDescriptors(tools []ToolDescriptor) []ToolDescriptor {
	out := make([]ToolDescriptor, len(tools))
	for i, desc := range tools {
		out[i] = ToolDescriptor{
			Name:        desc.Name,
			Description: desc.Description,
			InputSchema: append(json.RawMessage(nil), desc.InputSchema...),
		}
	}
	return out
}

// Submit admits one nested call on the VM owner goroutine (D13 step 3). It
// validates, counts and enqueues without touching the tool.
func (h hostAdapter) Submit(name string, args json.RawMessage) (uint64, *Failure) {
	e := h.e
	e.mu.Lock()
	if !e.admOpen || e.stopReq {
		e.mu.Unlock()
		return 0, &Failure{Kind: FailureInternal, Message: "execution no longer accepts calls"}
	}
	e.callsSubmitted++
	if e.callsSubmitted > e.limits.MaxCalls {
		e.registerCauseLocked(FailureCallLimit, fmt.Sprintf("more than %d submit attempts", e.limits.MaxCalls))
		e.stopLocked()
		e.mu.Unlock()
		return 0, &Failure{Kind: FailureCallLimit, Message: fmt.Sprintf("more than %d submit attempts; execution terminated", e.limits.MaxCalls)}
	}
	if !e.gen.allowSet[name] {
		e.mu.Unlock()
		return 0, &Failure{Kind: FailureNotFound, Message: fmt.Sprintf("tool %q is not exposed to this execution", name)}
	}
	if len(args) == 0 || args[0] != '{' || !json.Valid(args) || len(args) > e.limits.ArgsBytes {
		e.mu.Unlock()
		return 0, &Failure{Kind: FailureArgsInvalid, Message: fmt.Sprintf("arguments must be one JSON object within %d bytes", e.limits.ArgsBytes)}
	}
	e.pendingCalls++
	e.callSeq++
	id := e.callSeq
	select {
	case e.jobs <- hostJob{callID: id, name: name, args: copyRaw(args), acceptedAt: time.Now()}:
	default:
		e.pendingCalls--
		e.mu.Unlock()
		return 0, &Failure{Kind: FailureInternal, Message: "host job queue full"}
	}
	// queued is emitted while still holding e.mu so its sequence number is
	// assigned before any worker can emit approving/started for this job
	// (F17).
	e.mode.emit(CallEvent{ExecID: e.id, Generation: e.gen.version, CallID: id, Tool: name, State: CallQueued})
	e.mu.Unlock()
	return id, nil
}

// workerLoop pulls jobs until stop. Every job runs the exact 裁决 B ordering:
// resolve the live handle once, read Info from that same handle once,
// authorize from the live metadata, then execute the authorized handle.
func (e *execution) workerLoop() {
	for {
		select {
		case job := <-e.jobs:
			e.runJob(job)
		case <-e.stopCh:
			return
		case <-e.drainCh:
			return
		}
	}
}

func (e *execution) runJob(job hostJob) {
	e.mu.Lock()
	if e.stopReq || e.ctx.Err() != nil {
		e.pendingCalls--
		e.mu.Unlock()
		e.mode.emit(CallEvent{ExecID: e.id, Generation: e.gen.version, CallID: job.callID, Tool: job.name, State: CallCancelledBeforeStart})
		return
	}
	e.hostOutstanding++
	e.mu.Unlock()

	t, ok := e.source.Get(job.name)
	if !ok {
		e.rejectJob(job, &Failure{Kind: FailureNotFound, Message: fmt.Sprintf("tool %q not found", job.name)})
		return
	}
	if _, isWrapper := t.(*runCodeTool); isWrapper {
		// D7 recursion guard: no codemode wrapper of any instance is callable
		// from script, even under a custom name.
		e.rejectJob(job, &Failure{Kind: FailureNotFound, Message: "codemode wrappers cannot be called from scripts"})
		return
	}
	info, err := safeInfo(t)
	if err != nil {
		e.hardExit(err, "tool info failed")
		return
	}
	if info.Name != job.name {
		e.rejectJob(job, &Failure{Kind: FailureCatalogStale, Message: fmt.Sprintf("tool at %q reports a different name %q; Refresh required", job.name, info.Name)})
		return
	}
	liveSchema, err := json.Marshal(info.Parameters)
	if err != nil || string(liveSchema) != e.gen.paramsFP[job.name] {
		e.rejectJob(job, &Failure{Kind: FailureCatalogStale, Message: fmt.Sprintf("tool %q parameters changed since the advertised catalog; Refresh required", job.name)})
		return
	}

	if info.RequiresApproval {
		e.mode.emit(CallEvent{ExecID: e.id, Generation: e.gen.version, CallID: job.callID, Tool: job.name, State: CallApproving})
		if e.mode.approval == nil {
			e.rejectJob(job, &Failure{Kind: FailureDenied, Message: fmt.Sprintf("tool %q requires approval but no approval callback is configured", job.name)})
			return
		}
		release, ok := e.mode.acquireApproval(e.ctx)
		if !ok {
			e.cancelBeforeStart(job)
			return
		}
		// If cancellation and the slot were both ready, the select may have
		// granted the slot to an already-cancelled job: re-check before the
		// callback so a dying run never prompts the user (ocr medium).
		if e.ctx.Err() != nil {
			release()
			e.cancelBeforeStart(job)
			return
		}
		approved, aerr := safeApprove(e.mode.approval, *info, copyRaw(job.args))
		release()
		if aerr != nil {
			e.hardExit(aerr, "approval callback panicked")
			return
		}
		if !approved {
			e.rejectJob(job, &Failure{Kind: FailureDenied, Message: fmt.Sprintf("tool %q execution rejected by user", job.name)})
			return
		}
	}

	// Linearization point: the short lock decides whether this approved
	// call may start (裁决 B step 5). Cancel winning the lock forbids the
	// start; a cancel arriving after started is an in-flight cancellation
	// that cannot revoke admitted work.
	e.mu.Lock()
	if e.stopReq || e.ctx.Err() != nil {
		e.pendingCalls--
		e.hostOutstanding--
		e.mu.Unlock()
		e.mode.emit(CallEvent{ExecID: e.id, Generation: e.gen.version, CallID: job.callID, Tool: job.name, State: CallCancelledBeforeStart})
		return
	}
	e.mu.Unlock()
	e.mode.emit(CallEvent{ExecID: e.id, Generation: e.gen.version, CallID: job.callID, Tool: job.name, State: CallStarted})

	result, exErr := safeExecute(t, e.ctx, job.args)
	if exErr != nil {
		e.hardExit(exErr, fmt.Sprintf("tool %q failed", job.name))
		return
	}
	e.deliverResult(job, result)
}

// rejectJob delivers a recoverable rejection to the script and closes the
// job's host slot. The completion stays pending until its owner-side
// reservation (D13 step 5): a rejection the script can catch is still a
// delivery.
func (e *execution) rejectJob(job hostJob, failure *Failure) {
	e.mu.Lock()
	e.hostOutstanding--
	e.mu.Unlock()
	e.dropOrInbox(completion{callID: job.callID, failure: failure})
	e.mode.emit(CallEvent{ExecID: e.id, Generation: e.gen.version, CallID: job.callID, Tool: job.name, State: stateFor(failure.Kind)})
}

func (e *execution) cancelBeforeStart(job hostJob) {
	e.mu.Lock()
	e.pendingCalls--
	e.hostOutstanding--
	e.mu.Unlock()
	e.mode.emit(CallEvent{ExecID: e.id, Generation: e.gen.version, CallID: job.callID, Tool: job.name, State: CallCancelledBeforeStart})
}

// hardExit folds a worker-side hard fault into D12: it releases the host
// slot, selects the terminal cause when none is registered (the supervisor
// then returns the wrapped Go error) and is diagnostics-only afterwards.
func (e *execution) hardExit(err error, message string) {
	e.mu.Lock()
	e.hostOutstanding--
	e.mu.Unlock()
	e.raiseHard(err, message)
}

// raiseHard registers a hard fault without touching host-slot accounting
// (owner-side faults have no host slot). The cause is registered only if
// none was; the fault is buffered for the supervisor and the run is
// stopped either way.
func (e *execution) raiseHard(err error, message string) {
	e.mu.Lock()
	first := e.registerCauseLocked(FailureInternal, message)
	e.mu.Unlock()
	e.requestStop(FailureInternal, message)
	if first {
		// The pending flag is set before the buffered send so a scheduler
		// pause between stop and send can never downgrade a registered hard
		// fault to a soft response (ocr high).
		e.hardPending.Store(true)
		select {
		case e.hardErrCh <- hardFault{err: err, message: message}:
		default:
		}
	} else {
		e.observeLateMsg(message + " (after cause; diagnostics only)")
	}
}

// deliverResult bounds and converts one ToolResult, mints image tokens and
// hands the completion to the inbox (D5/D11.1). Cumulative budget overrun
// is terminal: nothing is delivered and the run stops.
func (e *execution) deliverResult(job hostJob, res *tool.ToolResult) {
	payload := resultPayloadBytes(res)
	e.mu.Lock()
	if payload > e.limits.HostResultBytes {
		e.hostOutstanding--
		e.mu.Unlock()
		e.rejectJobAfterSlot(job, &Failure{Kind: FailureBridgeLimit, Message: fmt.Sprintf("tool %q result is %d bytes, over the %d-byte single-result limit", job.name, payload, e.limits.HostResultBytes)})
		return
	}
	if e.hostDataBytes+payload > e.limits.HostDataBytes {
		e.registerCauseLocked(FailureBridgeLimit, fmt.Sprintf("cumulative bridged payload over %d bytes", e.limits.HostDataBytes))
		e.hostOutstanding--
		e.pendingCalls-- // terminal: this call is never delivered (F8c)
		e.stopLocked()
		e.mu.Unlock()
		e.mode.emit(CallEvent{ExecID: e.id, Generation: e.gen.version, CallID: job.callID, Tool: job.name, State: CallFinished})
		return
	}
	e.hostDataBytes += payload
	if e.imagesAccepted+len(res.Images) > e.limits.MaxImages {
		e.hostOutstanding--
		e.mu.Unlock()
		e.rejectJobAfterSlot(job, &Failure{Kind: FailureBridgeLimit, Message: fmt.Sprintf("tool %q would push the execution over the %d-image cap (%d already accepted + %d new); the result cannot be converted", job.name, e.limits.MaxImages, e.imagesAccepted, len(res.Images))})
		return
	}
	refs, err := e.mintImages(res.Images)
	if err != nil {
		e.hostOutstanding--
		e.mu.Unlock()
		e.rejectJobAfterSlot(job, &Failure{Kind: FailureBridgeLimit, Message: err.Error()})
		return
	}
	e.hostOutstanding--
	e.mu.Unlock()

	tv, terr := newToolValue(res, refs)
	if terr != nil {
		// Quota is consumed only after the result validates: an invalid Data
		// must not permanently burn image/payload budget (ocr medium).
		if f, ok := terr.(*Failure); ok {
			e.rejectJobAfterSlot(job, f)
			return
		}
		e.rejectJobAfterSlot(job, &Failure{Kind: FailureInternal, Message: terr.Error()})
		return
	}
	e.mu.Lock()
	e.imagesAccepted += len(res.Images)
	e.mu.Unlock()
	encoded, merr := json.Marshal(tv)
	if merr != nil {
		e.rejectJobAfterSlot(job, &Failure{Kind: FailureInternal, Message: "encode tool value"})
		return
	}
	comp := completion{callID: job.callID, value: encoded}
	if res.IsError() {
		// D11.1/D12: an errored tool result rejects the script-side promise
		// with {kind, message, result} so catch can branch on it.
		comp.failure = &Failure{
			Kind:    FailureToolError,
			Message: truncateMessage(res.Content),
			Value:   encoded,
		}
	}
	e.dropOrInbox(comp)
	e.mu.Lock()
	late := e.responseReturned || e.stopReq
	e.mu.Unlock()
	if late {
		e.mode.emit(CallEvent{ExecID: e.id, Generation: e.gen.version, CallID: job.callID, Tool: job.name, State: CallLateFinished, Duration: time.Since(job.acceptedAt)})
	} else {
		e.mode.emit(CallEvent{ExecID: e.id, Generation: e.gen.version, CallID: job.callID, Tool: job.name, State: CallFinished})
	}
}

// rejectJobAfterSlot enqueues a rejection for a path that already
// decremented hostOutstanding inside its locked section.
func (e *execution) rejectJobAfterSlot(job hostJob, failure *Failure) {
	e.dropOrInbox(completion{callID: job.callID, failure: failure})
	e.mode.emit(CallEvent{ExecID: e.id, Generation: e.gen.version, CallID: job.callID, Tool: job.name, State: stateFor(failure.Kind)})
}

// dropOrInbox hands a completion to the inbox without ever blocking a
// worker. After the delivery gate closed there is no owner left to reserve
// the data, so it is dropped and its pending slot settled right here (F8):
// every accepted call is settled exactly once, at reservation or drop.
func (e *execution) dropOrInbox(comp completion) {
	e.mu.Lock()
	if !e.deliveryOpen || e.stopReq {
		e.pendingCalls--
		e.mu.Unlock()
		return
	}
	select {
	case e.inbox <- comp:
		e.mu.Unlock()
	default:
		e.pendingCalls--
		e.mu.Unlock()
		e.observeLateMsg(fmt.Sprintf("completion %d dropped: inbox full", comp.callID))
	}
}

// mintImages copies image payloads under the images lock and returns opaque
// refs for this execution only.
func (e *execution) mintImages(imgs []tool.Image) ([]ImageRef, error) {
	e.imagesMu.Lock()
	defer e.imagesMu.Unlock()
	refs := make([]ImageRef, 0, len(imgs))
	for i, img := range imgs {
		e.imageSeq++
		token := fmt.Sprintf("%s-img-%d", e.id, e.imageSeq)
		e.images[token] = tool.Image{
			URL:      img.URL,
			MIMEType: img.MIMEType,
			Data:     append([]byte(nil), img.Data...),
		}
		refs = append(refs, ImageRef{Token: token})
		_ = i
	}
	return refs, nil
}

// lookupImage resolves one ref inside this execution; cross-execution or
// forged tokens fail here.
func (e *execution) lookupImage(token string) (tool.Image, bool) {
	e.imagesMu.Lock()
	defer e.imagesMu.Unlock()
	img, ok := e.images[token]
	return img, ok
}

// Reserve runs on the VM owner goroutine (D13 step 5): it blocks for one
// completion and, under the shared short lock, either reserves a delivery
// (incrementing DeliveriesInFlight and removing the call from PendingCalls)
// or drops the data because the gate already closed. A reserved completion
// must enter its one resolver call even if the gate closes afterwards.
func (h hostAdapter) Reserve() (CallReply, bool) {
	e := h.e
	for {
		var comp completion
		select {
		case comp = <-e.inbox:
		case <-e.stopCh:
			e.mu.Lock()
			open := e.deliveryOpen
			e.mu.Unlock()
			if !open {
				return CallReply{}, false
			}
			continue
		}
		e.mu.Lock()
		if !e.deliveryOpen {
			e.pendingCalls--
			e.mu.Unlock()
			continue
		}
		e.deliveriesInFlight++
		e.pendingCalls--
		reply := CallReply{CallID: comp.callID, Value: comp.value, Failure: comp.failure}
		e.mu.Unlock()
		return reply, true
	}
}

// FinishDelivery reports that the one resolver entry for callID returned or
// aborted; a delivery finishing after stop is metadata only.
func (h hostAdapter) FinishDelivery(callID uint64) {
	h.e.finishDelivery(callID)
}

// finishDelivery decrements the in-flight counter after the resolver entry
// returned or aborted; a delivery finishing after stop is metadata only.
func (e *execution) finishDelivery(callID uint64) {
	e.mu.Lock()
	e.deliveriesInFlight--
	late := e.responseReturned || e.stopReq
	e.mu.Unlock()
	if late {
		e.mode.emit(CallEvent{ExecID: e.id, Generation: e.gen.version, CallID: callID, State: CallLateDeliveryFinished})
	}
}

// registerCauseLocked selects the terminal cause exactly once; the
// first registration in this short lock wins (D12). Caller holds e.mu.
func (e *execution) registerCauseLocked(kind FailureKind, msg string) bool {
	if e.causeSet {
		return false
	}
	e.causeSet = true
	e.cause = kind
	e.causeMsg = msg
	return true
}

// requestStop performs the one-way stop transition inside the shared short
// critical section (C): close admission, cancel the run context, close the
// delivery gate, and fire the registered non-blocking interrupt. It never
// waits for user code. Re-registration of a cause after the first is
// diagnostics-only.
func (e *execution) requestStop(kind FailureKind, msg string) {
	e.mu.Lock()
	first := e.registerCauseLocked(kind, msg)
	e.stopLocked()
	e.mu.Unlock()
	if !first {
		e.observeLateMsg(fmt.Sprintf("cause %q (%s) registered after selected cause; diagnostics only", kind, msg))
	}
}

// stopLocked performs the stop transitions; caller holds e.mu. Idempotent:
// once stopped, later terminal causes and completion paths must not re-close
// stopCh (F2). Emitting under the lock is safe: the observer queue never
// blocks.
func (e *execution) stopLocked() {
	if e.stopReq {
		return
	}
	e.admOpen = false
	e.stopReq = true
	e.cancel()
	e.deliveryOpen = false
	close(e.stopCh)
	if e.interruptFn != nil {
		e.interruptFn()
	}
	e.mode.emit(CallEvent{ExecID: e.id, Generation: e.gen.version, State: CallStopping})
}

// observeLateMsg records a post-response or post-cause fact as execution-
// level metadata with the bounded diagnostic text (F17).
func (e *execution) observeLateMsg(message string) {
	e.mode.emit(CallEvent{ExecID: e.id, Generation: e.gen.version, State: CallLateFinished, Note: truncateMessage(message)})
}

// acquireApproval serializes nested approvals on one slot per CodeMode
// (D10). A cancelled run ctx releases the waiter; a job already inside the
// callback keeps it.
func (m *CodeMode) acquireApproval(ctx context.Context) (release func(), ok bool) {
	select {
	case m.approvalSlot <- struct{}{}:
		return func() { <-m.approvalSlot }, true
	case <-ctx.Done():
		return nil, false
	}
}

func stateFor(kind FailureKind) CallState {
	switch kind {
	case FailureNotFound:
		return CallNotFound
	case FailureDenied:
		return CallDenied
	case FailureCatalogStale:
		return CallCatalogStale
	}
	return CallFinished
}

// snapshots used by CodeMode.Snapshot; each reads under e.mu.
func (e *execution) hostOutstandingSnapshot() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.hostOutstanding
}

func (e *execution) deliveriesSnapshot() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.deliveriesInFlight
}

func (e *execution) vmNotExitedSnapshot() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.vmExited {
		return 0
	}
	return 1
}

// executionControl implements Control on top of the execution record.
type executionControl struct{ e *execution }

func (c *executionControl) RegisterInterrupt(fn func()) {
	e := c.e
	e.mu.Lock()
	if e.stopReq {
		e.mu.Unlock()
		fn()
		return
	}
	e.interruptFn = fn
	e.mu.Unlock()
}

func (c *executionControl) StopRequested() bool {
	c.e.mu.Lock()
	defer c.e.mu.Unlock()
	return c.e.stopReq
}

// ScriptSettled closes further submissions unconditionally (D13 step 6:
// after the top-level continuation, terminal-value encoding can still run
// script getters, and they must not admit new side effects); admitted calls
// that were never awaited select the unawaited_calls cause and stop the run.
func (c *executionControl) ScriptSettled() {
	e := c.e
	e.mu.Lock()
	if e.settled {
		e.mu.Unlock()
		return
	}
	e.settled = true
	e.admOpen = false
	if e.pendingCalls > 0 {
		e.registerCauseLocked(FailureUnawaited, fmt.Sprintf("script settled with %d admitted calls never awaited", e.pendingCalls))
		e.stopLocked()
	}
	e.mu.Unlock()
}

func (c *executionControl) PrintString(s string) (written int, exhausted bool) {
	e := c.e
	e.stdoutMu.Lock()
	defer e.stdoutMu.Unlock()
	if e.stdoutFull {
		return 0, true
	}
	remaining := e.limits.StdoutBytes - len(e.stdoutBuf)
	if remaining <= 0 {
		e.stdoutFull = true
		return 0, true
	}
	chunk := s
	if len(chunk) > remaining {
		chunk = cutAtRuneBoundary(chunk, remaining)
	}
	e.stdoutBuf = append(e.stdoutBuf, chunk...)
	written = len(chunk)
	if len(e.stdoutBuf) >= e.limits.StdoutBytes {
		e.stdoutFull = true
		exhausted = true
	}
	return written, exhausted
}

func cutAtRuneBoundary(s string, max int) string {
	if max >= len(s) {
		return s
	}
	cut := max
	for cut > 0 && !isRuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func isRuneStart(b byte) bool {
	return b&0xC0 != 0x80
}

func copyRaw(raw json.RawMessage) json.RawMessage {
	return append(json.RawMessage(nil), raw...)
}

// resultPayloadBytes measures one ToolResult the way the bridge budgets it:
// Content + Data bytes plus per-image payload, URL and MIME bytes and a
// fixed 128-byte per-image accounting overhead (D5).
func resultPayloadBytes(res *tool.ToolResult) int {
	n := len(res.Content) + len(res.Data)
	for _, img := range res.Images {
		n += len(img.Data) + len(img.URL) + len(img.MIMEType) + 128
	}
	return n
}

func safeInfo(t tool.Tool) (info *tool.ToolInfo, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("tool info panicked: %v", recovered)
		}
	}()
	i := t.Info()
	return &i, nil
}

func safeApprove(fn ApprovalFunc, info tool.ToolInfo, args json.RawMessage) (approved bool, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("approval callback panicked: %v", recovered)
		}
	}()
	return fn(info, args), nil
}

func safeExecute(t tool.Tool, ctx context.Context, args json.RawMessage) (result *tool.ToolResult, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			result = tool.NewErrorResult("tool panicked: %v", recovered)
			err = nil
		}
	}()
	res, exErr := t.Execute(ctx, args)
	if exErr != nil {
		return nil, exErr
	}
	if res == nil {
		return tool.NewErrorResult("tool returned nil result"), nil
	}
	return res, nil
}
