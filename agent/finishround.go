package agent

import (
	"context"
	"fmt"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
)

// FinishRoundAction is the decision a FinishRound hook returns.
type FinishRoundAction int

const (
	// FinishRoundDefault (the zero value) keeps the pre-steering scheduling:
	// tool calls continue, a tool-free reply ends the run.
	FinishRoundDefault FinishRoundAction = iota
	// FinishRoundContinue guarantees one more provider request. Steering
	// queued at the drain point rides along; without queued input the same
	// context is sent again. The guarantee is bounded by maxIter: on the
	// final allowed round the hook is not consulted at all.
	FinishRoundContinue
	// FinishRoundEnd cooperatively terminates the run with a normal Done
	// (Truncated=false). It is not a cancellation: no run_cancelled record
	// is written and SettleThread is not involved. Pending steering stays
	// queued for the next run.
	FinishRoundEnd
)

// FinishRoundDecision carries the hook's scheduling decision.
type FinishRoundDecision struct {
	Action FinishRoundAction
}

// FinishRoundContext is the completed round the hook inspects.
type FinishRoundContext struct {
	ThreadID  string
	Round     int           // zero-based logical round, same origin as RoundContextRequest.Round
	Assistant llm.Message   // the round's complete assistant message (tool calls included), deep-copied
	Results   []llm.Message // the round's ordered model-visible tool results, deep-copied
}

// FinishRoundError wraps a hook failure; the cause stays reachable through
// errors.Is/As and the run stays resumable.
type FinishRoundError struct {
	Round int
	Err   error
}

func (e *FinishRoundError) Error() string {
	return fmt.Sprintf("agent: finish round %d hook: %v", e.Round, e.Err)
}

func (e *FinishRoundError) Unwrap() error { return e.Err }

// WithFinishRound registers a cooperative round-boundary decision hook. It
// runs after a round's assistant message and tool results are durably
// committed and before the terminal branch is chosen — for tool rounds and
// tool-free rounds alike. Returning the zero decision keeps the default
// schedule. The hook is not consulted on the final allowed iteration (the
// maxIter terminal branch is frozen) nor on resumed cancelled snapshots.
// The hook runs serially within one run; concurrent runs of the same Agent
// must make the hook safe for concurrent use (same terms as
// RoundContextProvider).
func WithFinishRound(fn func(ctx context.Context, fc FinishRoundContext) (FinishRoundDecision, error)) Option {
	return func(a *Agent) { a.finishRound = fn }
}

// callFinishRound invokes the hook and normalizes its outcome. A hook error
// is wrapped in FinishRoundError for the session's interrupt wrapper.
func (a *Agent) callFinishRound(ctx context.Context, fc FinishRoundContext) (FinishRoundDecision, error) {
	if a.finishRound == nil {
		return FinishRoundDecision{}, nil
	}
	decision, err := a.finishRound(ctx, fc)
	if err != nil {
		return FinishRoundDecision{}, &FinishRoundError{Round: fc.Round, Err: err}
	}
	switch decision.Action {
	case FinishRoundDefault, FinishRoundContinue, FinishRoundEnd:
		return decision, nil
	default:
		return FinishRoundDecision{}, &FinishRoundError{Round: fc.Round,
			Err: fmt.Errorf("invalid finish round action %d", decision.Action)}
	}
}

// runClosedFor builds the cooperative terminal record for a
// FinishRound-End: the run closes without re-carrying the declared
// assistant message (it is already in history), mirroring how
// run_cancelled closes a run without inventing messages.
// copyMessage deep-copies one message (single-message variant of
// copyMessages).
func copyMessage(m llm.Message) llm.Message { return copyMessages([]llm.Message{m})[0] }

// trailingToolResults returns the trailing run of role=tool result messages
// at the end of history — exactly what the just-committed round appended,
// which is the hook's Results view.
func trailingToolResults(history []llm.Message) []llm.Message {
	end := len(history)
	start := end
	for start > 0 && history[start-1].Role == llm.RoleTool {
		start--
	}
	return copyMessages(history[start:end])
}

func runClosedFor(runID string, toolCalls int, usage, total llm.Usage) (store.Record, error) {
	rec, err := encodeRecord(store.KindRunClosed, recordID(runID, "-closed"),
		runClosedPayload{RunID: runID, ToolCalls: toolCalls, Usage: usage, TotalUsage: total})
	if err != nil {
		return store.Record{}, err
	}
	rec.Schema = store.SchemaV2
	return rec, nil
}
