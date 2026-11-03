package agent

import "fmt"

// RunInterruptedError identifies the persistent session whose error boundary
// an execution failure crossed. Unwrap exposes the original failure.
type RunInterruptedError struct {
	ThreadID string
	Err      error
	// Settlement binds this session before ownership is released. Wait for
	// the run to exit before using it; settling earlier fails fast with
	// ErrThreadBusy. Waiting removes the concurrency window but not write
	// uncertainty: an ambiguous write can leave this token behind the log,
	// and SettleThread then fails with ErrRevisionConflict. Such a failure
	// is final for this token — recover by taking a fresh SettlementTarget
	// as a new explicit recovery decision, or prefer the WithRunExitFn
	// token, which is sampled after all writes complete.
	Settlement *SettlementToken
}

func (e *RunInterruptedError) Error() string {
	return fmt.Sprintf("run on thread %q interrupted: %v", e.ThreadID, e.Err)
}

func (e *RunInterruptedError) Unwrap() error { return e.Err }

func wrapPersistentRunError(sess *persistence, err error) error {
	if err == nil {
		return nil
	}
	target := sess.settlementToken()
	return &RunInterruptedError{ThreadID: sess.thread, Err: err, Settlement: &target}
}
