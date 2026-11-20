package agent

import (
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
)

type measurementIdentity struct {
	Provider           string  `json:"provider"`
	Model              string  `json:"model"`
	RequestFingerprint *string `json:"request_fingerprint"`
	FingerprintVersion int     `json:"fingerprint_version"`
	PrefixMessages     int     `json:"prefix_messages"`
}

type declaredMeasurement struct {
	measurementIdentity
	Usage *llm.Usage `json:"usage"`
	// CaptureConfigSeq identifies the frozen request configuration. It makes
	// interrupted and uninterrupted folding agree across append absorption.
	CaptureConfigSeq int64 `json:"capture_config_seq"`
}

type doneMeasurement struct {
	measurementIdentity
	UsageReported    bool  `json:"usage_reported"`
	CaptureConfigSeq int64 `json:"capture_config_seq"`
}

// contextAnchorProjection is durable truth, without the runtime's rune caches.
type contextAnchorProjection struct {
	measurementIdentity
	InputTokens    int64  `json:"input_tokens"`
	OutputTokens   int64  `json:"output_tokens"`
	SourceRecordID string `json:"source_record_id"`
	SourceSeq      int64  `json:"source_seq"`
}

func measurementIdentityFor(m contextMeasurement, count int) measurementIdentity {
	var fingerprint *string
	if m.fingerprint != nil {
		value := hex.EncodeToString(m.fingerprint)
		fingerprint = &value
	}
	return measurementIdentity{
		Provider: m.provider, Model: m.model, RequestFingerprint: fingerprint,
		FingerprintVersion: m.fingerprintVersion, PrefixMessages: count,
	}
}

func (m measurementIdentity) measurement() contextMeasurement {
	var fingerprint []byte
	if m.RequestFingerprint != nil {
		// Wire identities have already passed validateMeasurementIdentity.
		fingerprint, _ = hex.DecodeString(*m.RequestFingerprint)
	}
	return contextMeasurement{
		provider: m.Provider, model: m.Model, fingerprint: fingerprint,
		fingerprintVersion: m.FingerprintVersion, prefixMessages: m.PrefixMessages,
	}
}

func (m measurementIdentity) matches(other measurementIdentity) bool {
	return m.RequestFingerprint != nil && other.RequestFingerprint != nil &&
		m.Provider == other.Provider && m.Model == other.Model &&
		m.FingerprintVersion == other.FingerprintVersion &&
		*m.RequestFingerprint == *other.RequestFingerprint
}

func (c configState) permitsMeasurement(m measurementIdentity) bool {
	return (c.Provider == nil || *c.Provider == m.Provider) &&
		(c.Model == nil || *c.Model == m.Model)
}

func (m measurementIdentity) invalidatedBy(p configChangedPayload) bool {
	return p.Thinking != nil || (p.Provider != nil && *p.Provider != m.Provider) ||
		(p.Model != nil && *p.Model != m.Model)
}

func invalidateProjection(anchor **contextAnchorProjection, p configChangedPayload) {
	if *anchor != nil && (*anchor).invalidatedBy(p) {
		*anchor = nil
	}
}

// responseAnchor validates identity even when usage cannot install a boundary.
func responseAnchor(anchor **contextAnchorProjection, m measurementIdentity, usage *llm.Usage, r store.Record) *contextAnchorProjection {
	if *anchor != nil && !m.matches((*anchor).measurementIdentity) {
		*anchor = nil
	}
	input, output, eligible := eligibleUsage(usage)
	if !eligible || m.RequestFingerprint == nil {
		return nil
	}
	return &contextAnchorProjection{
		measurementIdentity: m, InputTokens: input, OutputTokens: output,
		SourceRecordID: r.ID, SourceSeq: r.Seq,
	}
}

func requiredMeasurementFields(raw json.RawMessage, keys ...string) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	for _, key := range keys {
		value, present := fields[key]
		if !present || (string(value) == "null" && key != "request_fingerprint" && key != "usage") {
			return fmt.Errorf("context measurement requires %s", key)
		}
	}
	return nil
}

func validateMeasurementIdentity(raw json.RawMessage, m measurementIdentity) error {
	if err := requiredMeasurementFields(raw, "provider", "model", "request_fingerprint",
		"fingerprint_version", "prefix_messages"); err != nil {
		return err
	}
	if m.FingerprintVersion != 1 || m.PrefixMessages < 1 {
		return fmt.Errorf("invalid context measurement identity or boundary")
	}
	if m.RequestFingerprint != nil {
		value := *m.RequestFingerprint
		decoded, err := hex.DecodeString(value)
		if err != nil || len(decoded) != 32 || hex.EncodeToString(decoded) != value {
			return fmt.Errorf("invalid context measurement fingerprint")
		}
	}
	return nil
}

func measurementJSON(payload json.RawMessage) json.RawMessage {
	var fields map[string]json.RawMessage
	// The enclosing payload has already been decoded strictly.
	if err := json.Unmarshal(payload, &fields); err != nil {
		return nil
	}
	return fields["context_measurement"]
}

func validateDeclaredMeasurement(r store.Record, m *declaredMeasurement) error {
	if r.Schema != store.SchemaV5 {
		if m != nil {
			return incompatible(r, "context measurement requires schema 5")
		}
		return nil
	}
	if m == nil {
		return incompatible(r, "schema 5 declaration requires context_measurement")
	}
	raw := measurementJSON(r.Payload)
	if err := validateMeasurementIdentity(raw, m.measurementIdentity); err != nil {
		return incompatible(r, err.Error())
	}
	if err := requiredMeasurementFields(raw, "usage", "capture_config_seq"); err != nil {
		return incompatible(r, err.Error())
	}
	return nil
}

func validateDoneMeasurement(r store.Record, raw json.RawMessage, p doneEventPayload) error {
	m := p.ContextMeasurement
	if r.Schema != store.SchemaV5 {
		if m != nil {
			return incompatible(r, "context measurement requires schema 5")
		}
		return nil
	}
	var usage struct {
		Usage json.RawMessage `json:"usage"`
	}
	// A reported zero is an explicit object, never a missing or null report.
	if m != nil && m.UsageReported {
		if err := json.Unmarshal(raw, &usage); err != nil ||
			len(usage.Usage) == 0 || string(usage.Usage) == "null" {
			return incompatible(r, "reported done usage requires an object")
		}
	}
	raw = measurementJSON(raw)
	if p.Truncated {
		if string(raw) != "null" {
			return incompatible(r, "truncated done requires null context_measurement")
		}
		return nil
	}
	if m == nil {
		return incompatible(r, "schema 5 done requires context_measurement")
	}
	if err := validateMeasurementIdentity(raw, m.measurementIdentity); err != nil {
		return incompatible(r, err.Error())
	}
	if err := requiredMeasurementFields(raw, "usage_reported", "capture_config_seq"); err != nil {
		return incompatible(r, err.Error())
	}
	if !m.UsageReported && p.Usage != (llm.Usage{}) {
		return incompatible(r, "unreported done usage must be zero")
	}
	return nil
}

func (v *threadView) declarationAnchor(r store.Record, m *declaredMeasurement) (*contextAnchorProjection, error) {
	if m == nil {
		v.anchor = nil
		return nil, nil
	}
	count := len(v.history) + 1
	if v.system != "" && (len(v.history) == 0 || v.history[0].Role != llm.RoleSystem) {
		count++
	}
	if m.PrefixMessages != count {
		return nil, incompatible(r, "context measurement prefix does not match response position")
	}
	candidate := responseAnchor(&v.anchor, m.measurementIdentity, m.Usage, r)
	if !v.cfg.permitsMeasurement(m.measurementIdentity) {
		v.anchor = nil
		return nil, nil
	}
	if m.CaptureConfigSeq != v.cfgSeq {
		return nil, nil
	}
	return candidate, nil
}

func (v *threadView) appendOpenAssistant() {
	v.history = append(v.history, v.open.declared)
	if v.open.anchor != nil {
		v.anchor = v.open.anchor
	}
}

func (v *threadView) invalidateAnchors(p configChangedPayload) {
	if v.cfg.Provider == nil && p.Provider != nil {
		v.anchor = nil
		if v.open != nil {
			v.open.anchor = nil
		}
	}
	invalidateProjection(&v.anchor, p)
	if v.open != nil {
		invalidateProjection(&v.open.anchor, p)
	}
}
