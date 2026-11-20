package agent

import (
	"bytes"
	"fmt"

	"github.com/dailz1/go-agent/store"
)

func (v *threadView) foldContextRecovery(r store.Record, durable bool) error {
	p, err := decodeContextRecovery(r)
	if err != nil {
		return err
	}
	if !v.runActive || v.open != nil || p.RunID != v.runID || p.Round != v.lastRound+1 {
		return incompatible(r, "recovery does not identify the active run and next round")
	}
	var historyRunes int
	if p.Phase == "pending" || p.Phase == "preparing" {
		canonical := seededHistory(v.system, v.history)
		if durable {
			canonical, err = durableHistory(canonical)
			if err != nil {
				return incompatible(r, fmt.Sprintf("invalid durable recovery history: %v", err))
			}
		}
		historyRunes = estimateRunes(canonical)
	}
	old := v.recovery
	if old == nil {
		if p.Phase != "pending" || p.Step != 0 || p.Attempt != 0 || p.BudgetRunes != 0 {
			return incompatible(r, "episode must begin with pending step zero and no occupied attempt")
		}
		if p.ConfigSeq > v.cfgSeq {
			return incompatible(r, "recovery request config version is not yet folded")
		}
		if p.ConfigSeq >= 0 {
			if _, found := v.configRevisions[p.ConfigSeq]; !found || p.ConfigSeq >= r.Seq {
				return incompatible(r, "recovery request config version has no source record")
			}
		}
		if _, seen := v.episodes[p.EpisodeID]; seen {
			return incompatible(r, "episode id reused")
		}
	} else {
		if p.RunID != old.RunID || p.Round != old.Round || p.EpisodeID != old.EpisodeID ||
			p.Provider != old.Provider || p.Model != old.Model || p.ConfigSeq != old.ConfigSeq || p.Step != old.Step+1 {
			return incompatible(r, "recovery transition changes frozen identity or skips a step")
		}
		if p.Phase != "preparing" && (p.Attempt != old.Attempt || p.BudgetRunes != old.BudgetRunes) {
			return incompatible(r, "recovery transition changes occupied attempt or budget")
		}
		// Only a newly confirmed overflow updates the rejection baseline.
		// Preparing may use the current, already compacted history estimate.
		if p.Phase != "pending" && (p.Code != old.Code || p.RejectedOutboundRunes != old.RejectedOutboundRunes ||
			(p.Phase != "preparing" && p.BeforeRunes != old.BeforeRunes)) {
			return incompatible(r, "recovery transition changes the saved rejection baseline")
		}
		switch p.Phase {
		case "pending":
			if old.Phase != "dispatched" || old.Reason == "provider_error" {
				return incompatible(r, "pending requires a dispatched request without a failure note")
			}
			if p.RejectedOutboundRunes >= old.RejectedOutboundRunes {
				return incompatible(r, "pending rejection must be smaller than the previous outbound")
			}
		case "preparing":
			switch old.Phase {
			case "pending", "ready":
			case "preparing", "dispatched":
				want := "interrupted_attempt"
				if old.Phase == "dispatched" && old.Reason == "provider_error" {
					want = "resume_after_provider_error"
				}
				if p.Reason != want {
					return incompatible(r, "preparing does not identify the interrupted request cycle")
				}
			default:
				return incompatible(r, "preparing after a blocked episode")
			}
			if p.Attempt != old.Attempt+1 || p.Attempt > 2 ||
				p.BudgetRunes <= 0 || (old.BudgetRunes > 0 && p.BudgetRunes >= old.BudgetRunes) {
				return incompatible(r, "preparing must occupy the next bounded attempt with a lower budget")
			}
			if p.BeforeRunes != historyRunes || p.BudgetRunes >= p.BeforeRunes {
				return incompatible(r, "preparing budget does not shrink the current history")
			}
			if p.BudgetRunes > recoveryBudget(historyRunes, historyRunes, historyRunes, old.BudgetRunes) {
				return incompatible(r, "preparing budget exceeds the three-quarter ceiling")
			}
		case "dispatched":
			if old.Phase == "dispatched" {
				if old.Reason == "provider_error" || p.Reason != "provider_error" {
					return incompatible(r, "dispatched permits only one provider_error outcome note")
				}
			} else if old.Phase != "ready" || p.Reason == "provider_error" {
				return incompatible(r, "dispatch requires a committed ready history")
			}
		case "blocked":
			if old.Phase == "dispatched" && old.Reason == "provider_error" && old.Attempt < 2 &&
				p.Reason != "budget_exhausted" {
				return incompatible(r, "provider failure with a remaining slot requires explicit resume")
			}
			if p.Reason == "" || (old.Phase == "blocked" && p.Reason != old.Reason) {
				return incompatible(r, "blocked requires a stable reason")
			}
		case "superseded":
			if err := validateSuperseded(v.proof, r, p); err != nil {
				return err
			}
			v.recovery = nil
			return nil
		}
	}
	if p.Phase == "pending" && p.BeforeRunes != historyRunes {
		return incompatible(r, "pending history measurement does not match canonical history")
	}
	v.recovery = &recoveryProjection{
		RunID: p.RunID, Round: p.Round, EpisodeID: p.EpisodeID, Step: p.Step,
		Provider: p.Provider, Model: p.Model, ConfigSeq: p.ConfigSeq, Phase: p.Phase,
		Attempt: p.Attempt, BudgetRunes: p.BudgetRunes, BeforeRunes: p.BeforeRunes,
		RejectedOutboundRunes: p.RejectedOutboundRunes, Code: p.Code, Reason: p.Reason,
	}
	return nil
}

func (v *threadView) foldHistoryReplaced(r store.Record) error {
	p, err := decodeHistoryReplaced(r)
	if err != nil {
		return err
	}
	if !v.runActive || v.open != nil || p.RunID != v.runID || p.Round != v.lastRound+1 {
		return incompatible(r, "history replacement does not identify the active run and next round")
	}
	if prior, exists := v.replacements[p.OperationID]; exists {
		if prior.ID == r.ID && bytes.Equal(prior.Payload, r.Payload) {
			return nil // same immutable operation, never consumes another slot
		}
		return incompatible(r, "history replacement operation id reused")
	}
	original, err := durableHistory(seededHistory(v.system, v.history))
	if err != nil {
		return incompatible(r, fmt.Sprintf("invalid durable replacement history: %v", err))
	}
	if err := validateCompacted(original, p.History); err != nil {
		return incompatible(r, fmt.Sprintf("invalid replacement history: %v", err))
	}
	if p.Reason == "overflow_compaction" {
		old := v.recovery
		if old == nil || old.Phase != "preparing" || old.EpisodeID != p.EpisodeID ||
			old.Attempt != p.Attempt || old.BudgetRunes != p.BudgetRunes {
			return incompatible(r, "overflow replacement does not match the preparing attempt")
		}
		if p.BeforeRunes != old.BeforeRunes || p.BeforeRunes != estimateRunes(original) ||
			p.AfterRunes != estimateRunes(p.History) {
			return incompatible(r, "overflow replacement measurements do not match history")
		}
		next := *old
		next.Phase = "ready"
		v.recovery = &next
	} else if v.recovery != nil {
		return incompatible(r, "non-overflow replacement during an active recovery episode")
	}
	v.history = deepCopyMessages(p.History)
	v.legacyHistory = false
	if v.replacements == nil {
		v.replacements = make(map[string]store.Record)
	}
	v.replacements[p.OperationID] = r
	return nil
}

// checkRecoverySuccess runs before the lifecycle record changes any state.
// Clearing belongs to that same successful fold, after its other validation.
func checkRecoverySuccess(v threadView, r store.Record, round int) error {
	if v.recovery == nil {
		return nil
	}
	p := v.recovery
	if !v.runActive || p.RunID != v.runID || p.Round != round ||
		p.Phase != "dispatched" || p.Reason == "provider_error" {
		return incompatible(r, "successful reply does not match a live dispatched recovery request")
	}
	return nil
}
