package agent

import (
	"sort"

	"github.com/dailz1/go-agent/store"
)

// recoveryProofContext supplies the same bounded canonical prefix to full-log,
// checkpoint-tail and settlement-confirmation folds. Each proof additionally
// stops at its frozen selected version, never at the latest config projection.
func recoveryProofContext(v threadView, records []store.Record) threadView {
	v.proof = records
	return v
}

func validateSuperseded(records []store.Record, r store.Record, p contextRecoveryPayload) error {
	o, s := p.ConfigSeq, *p.SelectedConfigSeq
	if s >= r.Seq {
		return incompatible(r, "superseded selection is not before its record")
	}
	provider, model := p.Provider, p.Model
	next, lastChange := o+1, int64(-1)
	originalIsConfig := o == -1
	selectedIsConfig := false
	start := sort.Search(len(records), func(i int) bool { return records[i].Seq >= o })
	for _, record := range records[start:] {
		if record.Seq > s {
			break
		}
		if record.Seq == o && record.Kind == store.KindConfigChanged {
			if _, err := decodeConfigChanged(record); err != nil {
				return err
			}
			originalIsConfig = true
		}
		if record.Seq <= o {
			continue
		}
		if record.Seq != next {
			return incompatible(r, "superseded proof interval is incomplete")
		}
		next++
		if record.Schema < store.SchemaV1 || record.Schema > store.SchemaV5 {
			return incompatible(record, "unsupported schema in superseded proof")
		}
		if record.Kind == store.KindCheckpoint {
			if err := checkpointVersion(record); err != nil {
				return err
			}
		} else if record.Schema == store.SchemaV4 &&
			record.Kind != store.KindContextRecovery && record.Kind != store.KindHistoryReplaced {
			return incompatible(record, "schema 4 on a legacy record")
		}
		if record.Schema == store.SchemaV5 && record.Kind != store.KindCheckpoint &&
			record.Kind != store.KindRoundDeclared && record.Kind != store.KindAgentEvent {
			return incompatible(record, "schema 5 on a record without context measurement")
		}
		if record.Kind != store.KindConfigChanged {
			continue
		}
		change, err := decodeConfigChanged(record)
		if err != nil {
			return err
		}
		beforeProvider, beforeModel := provider, model
		if change.Provider != nil {
			provider = *change.Provider
		}
		if change.Model != nil {
			model = *change.Model
		}
		if provider != beforeProvider || model != beforeModel {
			lastChange = record.Seq
		}
		selectedIsConfig = record.Seq == s
	}
	if next != s+1 || !originalIsConfig || !selectedIsConfig || lastChange < 0 || lastChange != *p.ModelChangeSeq ||
		(provider == p.Provider && model == p.Model) ||
		provider != *p.SelectedProvider || model != *p.SelectedModel {
		return incompatible(r, "superseded selection is not proved by explicit config changes")
	}
	return nil
}
