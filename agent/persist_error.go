package agent

import "fmt"

// RunInterruptedError identifies the persistent session whose error boundary
// an execution failure crossed. Unwrap exposes the original failure.
type RunInterruptedError struct {
	ThreadID string
	Err      error
}

func (e *RunInterruptedError) Error() string {
	return fmt.Sprintf("run on thread %q interrupted: %v", e.ThreadID, e.Err)
}

func (e *RunInterruptedError) Unwrap() error { return e.Err }

func wrapPersistentRunError(threadID string, err error) error {
	if err == nil {
		return nil
	}
	return &RunInterruptedError{ThreadID: threadID, Err: err}
}
