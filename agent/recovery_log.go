package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
)

// durableHistory uses the same message codec as log replay. Measurements and
// adopted replacements must describe that form, including empty content slices.
func durableHistory(history []llm.Message) ([]llm.Message, error) {
	data, err := json.Marshal(history)
	if err != nil {
		return nil, fmt.Errorf("encode durable history: %w", err)
	}
	var decoded []llm.Message
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, fmt.Errorf("decode durable history: %w", err)
	}
	return decoded, nil
}

func (r *overflowRecovery) transition(phase, reason string) contextRecoveryPayload {
	s := r.state
	return contextRecoveryPayload{
		RunID: s.RunID, Round: s.Round, EpisodeID: s.EpisodeID, Step: s.Step + 1,
		Provider: s.Provider, Model: s.Model, ConfigSeq: s.ConfigSeq,
		Phase: phase, Reason: reason, Attempt: s.Attempt, BudgetRunes: s.BudgetRunes,
		BeforeRunes: s.BeforeRunes, RejectedOutboundRunes: s.RejectedOutboundRunes, Code: s.Code,
	}
}

func (r *overflowRecovery) save(ctx context.Context, history []llm.Message, payload contextRecoveryPayload, proof []store.Record) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p := r.sess
	if p != nil && r.state == nil {
		if err := p.anchorLegacyHistory(ctx, history); err != nil {
			return err
		}
	}
	record, err := encodeRecord(store.KindContextRecovery,
		fmt.Sprintf("%s-recovery-%d", payload.EpisodeID, payload.Step), payload)
	if err != nil {
		return err
	}
	record.Schema = store.SchemaV4
	v := threadView{runActive: true, runID: payload.RunID, lastRound: r.round - 1,
		history: history, recovery: r.state, cfgSeq: r.cfgSeq, proof: proof}
	if p != nil {
		record.Seq, v.cfgSeq = p.head, p.cfgSeq
		v.episodes, v.configRevisions = p.episodes, p.configRevisions
	}
	if err := v.foldContextRecovery(record, p != nil); err != nil {
		return err
	}
	if p != nil {
		if err := p.append(ctx, record); err != nil {
			return err
		}
		p.recovery = v.recovery
		if p.episodes == nil {
			p.episodes = make(map[string]struct{})
		}
		p.episodes[payload.EpisodeID] = struct{}{}
	}
	r.state = v.recovery
	r.a.logger.Debug("context recovery transition", "episode", payload.EpisodeID,
		"phase", payload.Phase, "attempt", payload.Attempt,
		"budget_runes", payload.BudgetRunes, "reason", payload.Reason)
	return nil
}

// replaceHistory is the canonical write for proactive, overflow and migration
// adoption. The existing fold checks the same payload before live adoption.
func (p *persistence) replaceHistory(ctx context.Context, original []llm.Message, payload historyReplacedPayload) ([]llm.Message, error) {
	record, err := encodeRecord(store.KindHistoryReplaced, payload.OperationID, payload)
	if err != nil {
		return nil, err
	}
	record.Schema, record.Seq = store.SchemaV4, p.head
	v := threadView{runActive: true, runID: p.runID, lastRound: p.lastRound,
		system: p.system, history: original, recovery: p.recovery}
	if err := v.foldHistoryReplaced(record); err != nil {
		return nil, err
	}
	if err := p.append(ctx, record); err != nil {
		return nil, err
	}
	p.recovery = v.recovery
	p.legacyHistory = false
	return v.history, nil
}

// A v3 snapshot has no canonical replacement. Anchor only when it was eligible
// for adoption; any v4 record in the observed prefix makes replay fall back.
func (p *persistence) anchorLegacyHistory(ctx context.Context, history []llm.Message) error {
	if !p.legacyHistory {
		return nil
	}
	_, err := p.replaceHistory(ctx, history, historyReplacedPayload{
		RunID: p.runID, Round: p.lastRound + 1,
		OperationID: fmt.Sprintf("%s-legacy-%d", p.runID, p.lastRound+1),
		Reason:      "legacy_anchor", History: history,
	})
	return err
}

// selectConfig only runs at a round entrance. A write may absorb later control
// records, but the proof and request both keep this entrance's frozen choice.
func (r *overflowRecovery) selectConfig(ctx context.Context, history []llm.Message) error {
	if r.state == nil {
		return nil
	}
	provider, model := r.provider.Name(), llm.ApplyOptions(r.opts).Model
	if provider == r.state.Provider && model == r.state.Model {
		return nil
	}
	p := r.transition("superseded", "config_changed")
	p.SelectedProvider, p.SelectedModel, p.SelectedConfigSeq = &provider, &model, &r.cfgSeq
	records, err := r.sess.a.store.History(ctx, r.sess.thread, max(0, r.state.ConfigSeq))
	if err != nil {
		return err
	}
	end := sort.Search(len(records), func(i int) bool { return records[i].Seq > r.cfgSeq })
	records = records[:end]
	derivedProvider, derivedModel, lastChange := r.state.Provider, r.state.Model, int64(-1)
	start := sort.Search(len(records), func(i int) bool { return records[i].Seq > r.state.ConfigSeq })
	for _, record := range records[start:] {
		if record.Kind != store.KindConfigChanged {
			continue
		}
		change, err := decodeConfigChanged(record)
		if err != nil {
			return err
		}
		beforeProvider, beforeModel := derivedProvider, derivedModel
		if change.Provider != nil {
			derivedProvider = *change.Provider
		}
		if change.Model != nil {
			derivedModel = *change.Model
		}
		if derivedProvider != beforeProvider || derivedModel != beforeModel {
			lastChange = record.Seq
		}
	}
	if lastChange < 0 || derivedProvider != provider || derivedModel != model {
		return r.failure("config_mismatch", nil)
	}
	p.ModelChangeSeq = &lastChange
	return r.save(ctx, history, p, records)
}

func (p *persistence) checkRecoveryRound(round int) error {
	return checkRecoverySuccess(threadView{
		runActive: true, runID: p.runID, recovery: p.recovery,
	}, store.Record{Seq: p.head}, round)
}
