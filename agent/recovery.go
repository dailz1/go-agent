package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/dailz1/go-agent/llm"
)

// ErrContextRecoveryFailed identifies an overflow recovery that cannot proceed.
var ErrContextRecoveryFailed = errors.New("agent: context recovery failed")

// ContextRecoveryError preserves the overflow and any local recovery failure.
// Attempts counts occupied compaction slots, not transport requests.
type ContextRecoveryError struct {
	Reason   string
	Attempts int
	Err      error
}

func (e *ContextRecoveryError) Error() string {
	return fmt.Sprintf("context recovery %s after %d attempt(s): %v", e.Reason, e.Attempts, e.Err)
}

func (e *ContextRecoveryError) Unwrap() error        { return e.Err }
func (e *ContextRecoveryError) Is(target error) bool { return target == ErrContextRecoveryFailed }

// overflowRecovery belongs to one logical round. Its request configuration
// never changes when a persistence write absorbs newer control records.
type overflowRecovery struct {
	a        *Agent
	sess     *persistence
	state    *recoveryProjection
	cause    error
	provider llm.Provider
	opts     []llm.Option
	window   int
	round    int
	cfgSeq   int64
}

func recoveryBudget(window, target, history, previous int) int {
	n := min(window, target, history)
	if previous > 0 {
		n = min(n, previous)
	}
	return n/4*3 + n%4*3/4
}

func (r *overflowRecovery) failure(reason string, err error) error {
	cause := r.cause
	if cause == nil && r.state != nil {
		cause = &llm.ContextOverflowError{Code: r.state.Code, Err: llm.ErrContextOverflow}
	}
	attempts := 0
	if r.state != nil {
		attempts = r.state.Attempt
	}
	return &ContextRecoveryError{Reason: reason, Attempts: attempts, Err: errors.Join(cause, err)}
}

func (r *overflowRecovery) canonicalHistory(history []llm.Message) ([]llm.Message, error) {
	if r.sess == nil {
		return deepCopyMessages(history), nil
	}
	return durableHistory(history)
}

func (r *overflowRecovery) block(ctx context.Context, history []llm.Message, reason string, err error) error {
	if cancel := ctx.Err(); cancel != nil {
		return cancel
	}
	p := r.transition("blocked", reason)
	if writeErr := r.save(ctx, history, p, nil); writeErr != nil {
		return errors.Join(r.failure(reason, err), writeErr)
	}
	return r.failure(reason, err)
}

func (r *overflowRecovery) rejected(ctx context.Context, history []llm.Message, outbound int, err error) error {
	r.cause = errors.Join(r.cause, err)
	var p contextRecoveryPayload
	if r.state == nil {
		id, idErr := newRunID()
		if idErr != nil {
			return idErr
		}
		runID := id
		if r.sess != nil {
			runID = r.sess.runID
		}
		p = contextRecoveryPayload{
			RunID: runID, Round: r.round, EpisodeID: id,
			Provider: r.provider.Name(), Model: llm.ApplyOptions(r.opts).Model,
			ConfigSeq: r.cfgSeq, Phase: "pending", Reason: "overflow",
		}
	} else {
		p = r.transition("pending", "overflow")
	}
	canonical, wireErr := r.canonicalHistory(history)
	if wireErr != nil {
		return wireErr
	}
	p.BeforeRunes, p.RejectedOutboundRunes = estimateRunes(canonical), outbound
	p.Code = ""
	var overflow *llm.ContextOverflowError
	if errors.As(err, &overflow) {
		p.Code = overflow.Code
	}
	return r.save(ctx, history, p, nil)
}

// prepare either reuses committed ready history or occupies a new slot before
// entering the compactor. A resumed preparing/dispatched slot is never reused.
func (r *overflowRecovery) prepare(ctx context.Context, history *[]llm.Message, yield func(AgentEvent, error) bool) bool {
	if r.state == nil {
		return true
	}
	stop := func(err error) bool { yield(nil, err); return false }
	if err := ctx.Err(); err != nil {
		return stop(err)
	}
	if !r.a.overflowRecovery {
		return stop(r.failure("disabled", nil))
	}
	if r.state.Phase == "blocked" {
		return stop(r.failure(r.state.Reason, nil))
	}
	canonical, err := r.canonicalHistory(*history)
	if err != nil {
		return stop(r.block(ctx, *history, "invalid_history", err))
	}
	before := estimateRunes(canonical)
	if r.state.Phase == "ready" && before <= min(r.state.BudgetRunes, r.window, r.a.targetRunesFor(r.window)) {
		return true
	}
	// A spent pending/dispatched cycle is exhausted even without a compactor.
	// A ready result needing a tighter budget retains F6's unavailable exit.
	if r.state.Attempt == 2 && (r.state.Phase != "ready" || r.a.compactor != nil) {
		return stop(r.block(ctx, *history, "attempts_exhausted", nil))
	}
	if r.a.compactor == nil {
		return stop(r.failure("unavailable", nil))
	}
	budget := recoveryBudget(r.window, r.a.targetRunesFor(r.window), before, r.state.BudgetRunes)
	if budget <= 0 {
		return stop(r.block(ctx, *history, "budget_exhausted", nil))
	}
	if hasUnsupportedBlocks(*history) {
		return stop(r.block(ctx, *history, "invalid_history", ErrInvalidHistory))
	}
	reason := "overflow"
	switch r.state.Phase {
	case "preparing", "dispatched":
		reason = "interrupted_attempt"
		if r.state.Reason == "provider_error" {
			reason = "resume_after_provider_error"
		}
	case "ready":
		reason = "budget_changed"
	}
	p := r.transition("preparing", reason)
	p.Attempt++
	p.BudgetRunes, p.BeforeRunes = budget, before
	if err := r.save(ctx, *history, p, nil); err != nil {
		return stop(err)
	}
	if err := ctx.Err(); err != nil {
		return stop(err)
	}
	result, err := r.a.compactor.Compact(ctx, deepCopyMessages(*history),
		CompactionBudget{MaxRunes: budget}, r.provider, append([]llm.Option(nil), r.opts...))
	if cancel := ctx.Err(); cancel != nil {
		return stop(cancel)
	}
	if err != nil {
		return stop(r.block(ctx, *history, "compactor_failed", err))
	}
	if len(result.History) == 0 || hasUnsupportedBlocks(result.History) {
		return stop(r.block(ctx, *history, "invalid_compaction", ErrInvalidHistory))
	}
	adopted, err := r.canonicalHistory(result.History)
	if err != nil {
		return stop(r.block(ctx, *history, "invalid_compaction", err))
	}
	if err := validateCompacted(canonical, adopted); err != nil {
		return stop(r.block(ctx, *history, "invalid_compaction", err))
	}
	after := estimateRunes(adopted)
	if !result.Changed || after >= before {
		return stop(r.block(ctx, *history, "no_progress", nil))
	}
	if after > budget {
		return stop(r.block(ctx, *history, "budget_exceeded", ErrCompactionBudgetExceeded))
	}
	if r.sess != nil {
		adopted, err = r.sess.replaceHistory(ctx, *history, historyReplacedPayload{
			RunID: r.state.RunID, Round: r.round,
			OperationID: fmt.Sprintf("%s-history-%d", r.state.EpisodeID, r.state.Attempt),
			Reason:      "overflow_compaction", History: adopted,
			EpisodeID: r.state.EpisodeID, Attempt: r.state.Attempt, BudgetRunes: budget,
			BeforeRunes: before, AfterRunes: after,
		})
		if err != nil {
			return stop(err)
		}
		r.state = r.sess.recovery
	} else {
		next := *r.state
		next.Phase = "ready"
		r.state = &next
	}
	*history = adopted
	return yield(CompactionEvent{
		Strategies: append([]string(nil), result.Strategies...), DroppedGroups: result.DroppedGroups,
		BeforeRunes: before, AfterRunes: after, History: deepCopyMessages(adopted),
	}, nil)
}

// beforeDispatch runs after the callback on every direct provider invocation.
// Only the first request of the cycle changes ready to dispatched.
func (r *overflowRecovery) beforeDispatch(ctx context.Context, history []llm.Message, outbound int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.state == nil {
		return nil
	}
	if outbound >= r.state.RejectedOutboundRunes {
		return r.block(ctx, history, "no_progress", nil)
	}
	if r.state.Phase == "ready" {
		return r.save(ctx, history, r.transition("dispatched", "request"), nil)
	}
	return nil
}

func (r *overflowRecovery) providerFailed(ctx context.Context, history []llm.Message, err error) error {
	if r.state == nil || r.state.Phase != "dispatched" {
		return err
	}
	if writeErr := r.save(ctx, history, r.transition("dispatched", "provider_error"), nil); writeErr != nil {
		return errors.Join(err, writeErr)
	}
	return err
}
