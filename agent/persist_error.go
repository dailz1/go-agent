package agent

import "fmt"

// RunInterruptedError identifies the persistent session whose error boundary
// an execution failure crossed. Unwrap exposes the original failure.
type RunInterruptedError struct {
	ThreadID string
	Err      error
	// Settlement binds this session before ownership is released. Wait for
	// the run to exit before using it; an uncertain write can leave it stale.
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
