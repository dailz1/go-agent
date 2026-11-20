package agent

import (
	"encoding/json"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
)

// stagedMeasurement belongs to the successful request, not the subsequently
// absorbed configuration. The payload remains immutable while append rebases.
type stagedMeasurement struct {
	payload declaredMeasurement
	history []llm.Message
}

func (p *persistence) stageMeasurement(m contextMeasurement, usage *llm.Usage, history []llm.Message) {
	var copied *llm.Usage
	if usage != nil {
		value := *usage
		copied = &value
	}
	identity := measurementIdentityFor(m, len(history))
	p.measurement = &stagedMeasurement{
		payload: declaredMeasurement{measurementIdentity: identity, Usage: copied, CaptureConfigSeq: m.captureConfigSeq},
		history: history,
	}
}

func (p *persistence) measurementPayload() *declaredMeasurement {
	if p.measurement == nil {
		return nil
	}
	return &p.measurement.payload
}

func (p *persistence) confirmMeasurement(rec store.Record) {
	pending := p.measurement
	if pending == nil {
		return
	}
	m := pending.payload
	// append supplies the response's actual sequence and confirms it before
	// folding any control records after that position.
	candidate := responseAnchor(&p.anchor, m.measurementIdentity, m.Usage, rec)
	if !p.cfg.permitsMeasurement(m.measurementIdentity) {
		p.anchor = nil
		if p.context != nil {
			p.context.invalidate("config_override")
		}
	} else {
		if candidate != nil && m.CaptureConfigSeq == p.cfgSeq {
			p.anchor = candidate
		}
		if p.context != nil {
			usage := m.Usage
			if m.CaptureConfigSeq != p.cfgSeq {
				usage = nil
			}
			p.context.activateMeasurement(m.measurementIdentity.measurement(), usage, pending.history, nil)
		}
	}
	p.measurement = nil
}

func (p *persistence) invalidateAnchors(change configChangedPayload) {
	if p.cfg.Provider == nil && change.Provider != nil {
		p.anchor = nil
		if p.context != nil {
			p.context.invalidate("provider_route_changed")
		}
	}
	invalidateProjection(&p.anchor, change)
	if p.context != nil {
		p.context.configChanged(change)
	}
}

func (c *contextRuntime) restoreAnchor(p *contextAnchorProjection, history []llm.Message) {
	if p == nil || p.Provider == "" || p.Model == "" || p.PrefixMessages > len(history) {
		return
	}
	m := p.measurementIdentity.measurement()
	m.inputTokens, m.outputTokens = p.InputTokens, p.OutputTokens
	c.anchor = newUsageAnchor(m, history[:p.PrefixMessages], &c.generation)
	if c.anchor != nil {
		c.anchor.durable = c.durable
	}
}

// Message.UnmarshalJSON represents null content as an empty slice. Measure
// that durable representation without changing the provider's live history.
// Anchored calls pass only their appended suffix, never rescan the prefix.
func measurementHistory(history []llm.Message) []llm.Message {
	for i, message := range history {
		if message.Content != nil {
			continue
		}
		normalized := append([]llm.Message(nil), history...)
		for j := i; j < len(normalized); j++ {
			if normalized[j].Content == nil {
				normalized[j].Content = []llm.ContentBlock{}
			}
		}
		return normalized
	}
	return history
}

func validCheckpointAnchor(payload json.RawMessage, anchor *contextAnchorProjection) bool {
	var fields struct {
		ContextAnchor json.RawMessage `json:"context_anchor"`
	}
	if err := json.Unmarshal(payload, &fields); err != nil {
		return false
	}
	raw := fields.ContextAnchor
	if len(raw) == 0 {
		return false
	}
	if anchor == nil {
		return string(raw) == "null"
	}
	if err := validateMeasurementIdentity(raw, anchor.measurementIdentity); err != nil {
		return false
	}
	if err := requiredMeasurementFields(raw, "input_tokens", "output_tokens", "source_record_id", "source_seq"); err != nil {
		return false
	}
	_, _, eligible := eligibleUsage(&llm.Usage{InputTokens: anchor.InputTokens, OutputTokens: anchor.OutputTokens})
	return eligible && anchor.RequestFingerprint != nil && anchor.SourceRecordID != "" && anchor.SourceSeq >= 0
}
