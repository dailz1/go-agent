package codemode

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	"github.com/dailz1/go-agent/tool"
)

// runCodeTool is the single model-facing tool bound to one immutable
// generation (D9). It never reads the CodeMode's current pointer at
// execution time; Refresh swaps whole wrappers, so an in-flight g1 wrapper
// keeps running with g1's catalog, allow-set and declarations.
type runCodeTool struct {
	mode *CodeMode
	gen  *generation
}

func (t *runCodeTool) Info() tool.ToolInfo {
	return tool.ToolInfo{
		Name:             t.mode.name,
		Description:      t.gen.description,
		Parameters:       outerParameters(),
		RequiresApproval: t.mode.outerApprovalRequired,
	}
}

// outerParameters is fixed for every generation: exactly code and
// catalog_version, no additional properties (D8). Limits are not exposed to
// the model.
func outerParameters() tool.ParameterSchema {
	ps := tool.NewParameterSchema()
	ps.Properties["code"] = tool.Property{
		Type:        "string",
		Description: "Script source in the documented entry syntax.",
	}
	ps.Properties["catalog_version"] = tool.Property{
		Type:        "string",
		Description: "Copy the catalog version string from this tool's description.",
	}
	ps.Required = []string{"code", "catalog_version"}
	noExtra := false
	ps.AdditionalProperties = &noExtra
	return ps
}

type outerArgs struct {
	Code           string `json:"code"`
	CatalogVersion string `json:"catalog_version"`
}

func (t *runCodeTool) Execute(ctx context.Context, args json.RawMessage) (*tool.ToolResult, error) {
	if ctx.Value(nestedExecKey{}) != nil {
		// D7 recursion guard: a call that carries a codemode execution marker
		// is already running inside a script; alias wrappers forwarding ctx
		// cannot re-enter.
		return tool.NewErrorResult("recursive codemode call is rejected"), nil
	}
	var in outerArgs
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return tool.NewErrorResult("run_code arguments must be exactly {code, catalog_version}: %v", err), nil
	}
	if dec.More() {
		// Trailing JSON values or garbage after the object must be rejected,
		// not silently ignored. A second Decode is the reliable probe: a
		// well-formed stream ends with io.EOF, and Decoder.More can report
		// false for an invalid trailing token (ocr medium).
		var extra json.RawMessage
		if err := dec.Decode(&extra); err == nil {
			return tool.NewErrorResult("run_code arguments must be exactly one {code, catalog_version} object"), nil
		}
	}
	if in.Code == "" {
		return tool.NewErrorResult("run_code requires a non-empty code argument"), nil
	}
	if len(in.Code) > t.mode.cfg.sourceBytes {
		return tool.NewErrorResult("script source is %d bytes, over the %d-byte limit", len(in.Code), t.mode.cfg.sourceBytes), nil
	}
	if in.CatalogVersion != t.gen.version {
		// D2-1 boundary: the rejection carries the wrapper's own version
		// only — never another generation's declarations.
		return tool.NewErrorResult(
			"catalog_version %q is not current for this description (%s); re-read the run_code description and resubmit with its catalog_version",
			in.CatalogVersion, t.gen.version), nil
	}
	m := t.mode
	// WallClock covers the whole accepted execution, starting before any
	// goroutine is spawned (D6).
	wall := time.NewTimer(m.cfg.wallClock)
	m.liveMu.Lock()
	if m.closed.Load() {
		m.liveMu.Unlock()
		wall.Stop()
		return tool.NewErrorResult("run_code is closed"), nil
	}
	live := len(m.live)
	if live >= m.cfg.maxLiveRuns {
		m.liveMu.Unlock()
		wall.Stop()
		return tool.NewErrorResult("run_code is busy: %d executions are still live (MaxLiveRuns)", live), nil
	}
	e := m.newExecution(t, ctx, in.Code, wall)
	m.live[e] = struct{}{}
	m.lifeWG.Add(1)
	m.liveMu.Unlock()

	resp := e.supervise()
	return resp.result, resp.hardErr
}

// supervise drives one execution to its response under C's three clocks and
// D12's first-registered-cause rule; it then keeps supervising in the
// background until real cleanup completes. Execute returns as soon as the
// response is settled.
func (e *execution) supervise() execResponse {
	ownerDone := e.ownerDone

	// Hard faults win whenever they are already buffered: the cause was
	// registered before the clock/cancel stop ran (F5).
	select {
	case h := <-e.hardErrCh:
		return e.hardResponse(h)
	default:
	}

	select {
	case res := <-ownerDone:
		e.onRunReturned(res)
		e.drainWorkers()
	case <-e.ctx.Done():
		e.requestStop(FailureCancelled, "parent context cancelled")
		e.awaitBoundedDrain()
	case <-e.wall.C:
		e.requestStop(FailureTimeout, "wall clock deadline")
		e.awaitBoundedDrain()
	case h := <-e.hardErrCh:
		// Hard error before any terminal cause interrupts the Agent round
		// (D12); stop everything, then propagate.
		return e.hardResponse(h)
	}

	// A hard fault may have landed during the drain window; it registered
	// the first cause, so it wins over the assembled soft response (D12).
	if h, ok := e.takeHardFault(); ok {
		return e.hardResponse(h)
	}

	// The response settles here; cleanup may still be draining and keeps
	// its ledger slot (C). The response_returned event is emitted before
	// the ledger slot can be released, so Close can never close the queue
	// under a pending emit (F4).
	resp := e.assembleResponse()
	e.mode.emit(CallEvent{ExecID: e.id, Generation: e.gen.version, State: CallResponseReturned})
	e.mu.Lock()
	e.responseReturned = true
	e.mu.Unlock()

	if !resp.cleanupIncomplete {
		// Finalize only after owner and workers have actually joined: the
		// counter snapshot alone does not prove the pool exited (ocr high).
		go e.awaitRealExit(false)
	} else {
		go e.awaitRealExit(true)
	}
	return resp
}

// hardResponse turns a buffered hard fault into the outer Go error via the
// bounded-category wrapper (D12/F15), releases response bookkeeping, and
// keeps the ledger slot until real cleanup (F3).
func (e *execution) hardResponse(h hardFault) execResponse {
	e.raiseHard(h.err, h.message)
	e.mode.emit(CallEvent{ExecID: e.id, Generation: e.gen.version, State: CallResponseReturned})
	e.mu.Lock()
	e.responseReturned = true
	e.mu.Unlock()
	go e.awaitRealExit(true)
	return execResponse{hardErr: &hardAgentError{category: h.message, cause: h.err}}
}

// takeHardFault drains a buffered fault without blocking.
func (e *execution) takeHardFault() (hardFault, bool) {
	select {
	case h := <-e.hardErrCh:
		e.hardPending.Store(false)
		return h, true
	default:
		return hardFault{}, false
	}
}

// onRunReturned folds a completed Executor.Run into the cause set: script
// and encoding failures are registrable causes (D12) competing in the same
// short lock with deadline/cancel/host faults.
func (e *execution) onRunReturned(res runResult) {
	e.mu.Lock()
	e.vmExited = true
	e.mu.Unlock()
	if res.err != nil {
		e.hardExit(res.err, "executor infrastructure fault")
		return
	}
	e.mu.Lock()
	e.outcome = &res.outcome
	e.mu.Unlock()
	f := res.outcome.Failure
	if f == nil {
		return
	}
	switch f.Kind {
	case FailureScript, FailureEncoding:
		e.requestStop(f.Kind, f.Message)
	default:
		e.hardExit(nil, f.Message)
	}
}

// drainWorkers releases the worker pool after the owner already returned:
// by then the script settled, so no new submissions are possible and the job
// queue is empty (unawaited calls stopped the run before reaching here);
// workers exit through the drain gate instead of lingering until Close.
func (e *execution) drainWorkers() {
	e.drainOnce.Do(func() { close(e.drainCh) })
	hostTimer := time.NewTimer(e.limits.HostDrainTimeout)
	defer hostTimer.Stop()
	drained := make(chan struct{})
	go func() {
		e.workersWG.Wait()
		e.drainAbandonedJobs()
		close(drained)
	}()
	select {
	case <-drained:
	case <-hostTimer.C:
		e.observeLateMsg("host drain wait exhausted")
	}
}

// awaitBoundedDrain runs the VM-exit wait and the host-drain wait in
// parallel, each with its own clock, so the response waits at most
// max(VMExitTimeout, HostDrainTimeout) after the stop, never their sum
// (C/F6). Each wait's own expiry clears its pending flag; the overall bound
// returns early. It never claims a stopped VM or drained host that has not
// stopped.
func (e *execution) awaitBoundedDrain() {
	type vmSignal struct {
		res    runResult
		hasRes bool
	}
	vmDone := make(chan vmSignal, 1)
	go func() {
		vmTimer := time.NewTimer(e.limits.VMExitTimeout)
		defer vmTimer.Stop()
		sig := vmSignal{}
		select {
		case res := <-e.ownerDone:
			sig.res, sig.hasRes = res, true
		case <-vmTimer.C:
			e.observeLateMsg("vm exit wait exhausted")
		}
		vmDone <- sig
	}()
	hostDone := make(chan struct{})
	go func() {
		hostTimer := time.NewTimer(e.limits.HostDrainTimeout)
		defer hostTimer.Stop()
		e.workersWG.Wait()
		e.drainAbandonedJobs()
		close(hostDone)
	}()
	bound := maxDuration(e.limits.VMExitTimeout, e.limits.HostDrainTimeout)
	boundTimer := time.NewTimer(bound)
	defer boundTimer.Stop()
	vmWaiting, hostWaiting := true, true
	var late runResult
	for vmWaiting || hostWaiting {
		select {
		case sig := <-vmDone:
			if sig.hasRes {
				late = sig.res
			}
			vmWaiting = false // its own expiry or delivery clears the flag (F6)
		case <-hostDone:
			hostWaiting = false
		case <-boundTimer.C:
			e.observeLateMsg("bounded drain wait exhausted")
			return
		}
	}
	if late.err != nil || late.outcome.Failure != nil || late.outcome.Value != nil || late.outcome.ValueOmitted {
		e.onRunReturned(late)
	}
}

// drainAbandonedJobs settles the pendingCalls of jobs still queued when the
// workers exited on stop: they are cancelled before start and never execute
// (F8).
func (e *execution) drainAbandonedJobs() {
	for {
		select {
		case job := <-e.jobs:
			e.mu.Lock()
			e.pendingCalls--
			e.mu.Unlock()
			e.mode.emit(CallEvent{ExecID: e.id, Generation: e.gen.version, CallID: job.callID, Tool: job.name, State: CallCancelledBeforeStart})
		default:
			return
		}
	}
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

// awaitRealCleanup runs after a cleanup_incomplete response: it waits for
// the VM, reserved deliveries and host work to really exit, then reports
// cleanup_complete and releases the ledger slot. There is no deadline on
// this wait — Close waits with it (C).
func (e *execution) awaitRealCleanup() {
	e.awaitRealExit(true)
}

// awaitRealExit waits for the owner goroutine and the worker pool to
// actually join, marks cleanup complete, optionally reports the
// cleanup_complete event, and only then releases the ledger slot: the
// counter snapshot used for the response does not prove the pool exited
// (ocr high).
func (e *execution) awaitRealExit(reportCleanup bool) {
	e.ownerWG.Wait()
	e.workersWG.Wait()
	e.drainAbandonedJobs()
	e.cancel() // release ctx resources on every exit path, success included
	e.mu.Lock()
	e.cleanupComplete = true
	e.mu.Unlock()
	if reportCleanup {
		e.mode.emit(CallEvent{ExecID: e.id, Generation: e.gen.version, State: CallCleanupComplete})
	}
	e.finalizeLedger()
}

// finalizeLedger releases the live slot; late events after this point only
// hit the observer, never the returned ToolResult.
func (e *execution) finalizeLedger() {
	m := e.mode
	m.liveMu.Lock()
	delete(m.live, e)
	m.liveMu.Unlock()
	m.lifeWG.Done()
}
