package agent

import (
	"context"
	"fmt"

	"github.com/dailz1/go-agent/store"
)

// SettlementToken identifies one run at its last confirmed durable revision.
// Save it with the Agent/Store that issued it. It is not an authorization token.
// Never refresh an old cancellation request to target a successor run.
type SettlementToken struct {
	ThreadID     string
	RunID        string
	ExpectedHead int64
}

// WithRunExitFn observes each established persistent session after its owned
// work exits but before ownership is released, including error and early-break
// exits. The callback must only save the value, not reenter thread operations
// or wait for ownership. Hosts must synchronize concurrent callbacks.
func WithRunExitFn(fn func(SettlementToken)) Option {
	return func(a *Agent) { a.runExitFn = fn }
}

func (p *persistence) settlementToken() SettlementToken {
	return SettlementToken{ThreadID: p.thread, RunID: p.runID, ExpectedHead: p.head}
}

// SettlementTarget inspects the current incomplete run for a new explicit
// recovery decision. It never executes or writes. It must not be used to
// refresh the identity or revision of an earlier cancellation request.
func (a *Agent) SettlementTarget(ctx context.Context, threadID string) (*SettlementToken, error) {
	if err := a.acquireSettlement(ctx, threadID); err != nil {
		return nil, err
	}
	defer releaseThread(a.store, threadID)
	v, err := a.replayThread(ctx, threadID)
	if err != nil {
		return nil, err
	}
	if !v.runActive {
		return nil, ErrNothingToSettle
	}
	return &SettlementToken{ThreadID: threadID, RunID: v.runID, ExpectedHead: v.head}, nil
}

// SettleThread explicitly cancels the precise incomplete run bound by target.
// First cancel and join its executor, then use a fresh bounded context and the
// original token. Settlement neither signals cancellation nor waits for work.
// It atomically pairs any open declaration with unknown results and closes the
// run, without executing tools, producing Done, or rolling back side effects.
//
// A lost acknowledgement may be retried with the same token. Such a retry
// confirms only the original cancellation, never cancels a successor run.
// Settlement and accepting new input are separate durability boundaries.
func (a *Agent) SettleThread(ctx context.Context, target SettlementToken) error {
	if err := a.acquireSettlement(ctx, target.ThreadID); err != nil {
		return err
	}
	defer releaseThread(a.store, target.ThreadID)
	if target.RunID == "" || target.ExpectedHead < 1 {
		return store.ErrRevisionConflict
	}
	v, err := a.replayThread(ctx, target.ThreadID)
	if err != nil {
		return err
	}
	if v.head > target.ExpectedHead {
		return a.confirmSettlement(ctx, target)
	}
	if v.head != target.ExpectedHead || v.runID != target.RunID {
		return store.ErrRevisionConflict
	}
	if !v.runActive {
		return ErrNothingToSettle
	}
	rec, err := v.cancelRecord()
	if err != nil {
		return err
	}
	if _, err := a.store.Append(ctx, target.ThreadID, target.ExpectedHead, rec); err != nil {
		return fmt.Errorf("agent: settle thread %q: %w", target.ThreadID, err)
	}
	return nil
}

func (a *Agent) acquireSettlement(ctx context.Context, threadID string) error {
	if a.store == nil {
		return ErrNoStore
	}
	if err := validateThreadID(threadID); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return acquireThread(a.store, threadID)
}

func (a *Agent) confirmSettlement(ctx context.Context, target SettlementToken) error {
	records, err := a.store.History(ctx, target.ThreadID, 0)
	if err != nil {
		return fmt.Errorf("agent: confirm settlement: %w", err)
	}
	h := target.ExpectedHead
	if h >= int64(len(records)) || records[h].Seq != h ||
		records[h].Kind != store.KindRunCancelled || records[h].ID != recordID(target.RunID, "-cancel") {
		return store.ErrRevisionConflict
	}
	prefix, err := replayRecords(threadView{lastRound: -1}, records[:h])
	if err != nil {
		return err
	}
	if !prefix.runActive || prefix.runID != target.RunID {
		return store.ErrRevisionConflict
	}
	if _, err := replayRecords(prefix, records[h:h+1]); err != nil {
		return err
	}
	if _, err := a.store.Append(ctx, target.ThreadID, h, records[h]); err != nil {
		return fmt.Errorf("agent: confirm settlement: %w", err)
	}
	return nil
}
