package agent

import (
	"bytes"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

// contextRuntime is owned by one running iterator, including its ephemeral
// runs. Durable anchors with a known model may be restored across invocations.
type contextRuntime struct {
	durable            bool
	anchor             *usageAnchor
	generation         uint64
	invalidationReason string
	overlay            *overlayFitter
	overlayGeneration  uint64
}

func (c *contextRuntime) invalidate(reason string) {
	if c.anchor != nil {
		c.invalidationReason = reason
		c.anchor = nil
	}
}

func (c *contextRuntime) replaceHistory() {
	c.generation++
	c.invalidate("history_replaced")
}

func (c *contextRuntime) checkIdentity(m contextMeasurement) {
	if c.anchor == nil {
		return
	}
	switch {
	case c.anchor.provider != m.provider:
		c.invalidate("provider_changed")
	case c.anchor.model != m.model:
		c.invalidate("model_changed")
	case m.fingerprint == nil || !bytes.Equal(c.anchor.fingerprint, m.fingerprint):
		c.invalidate("fingerprint_changed")
	}
}

func (c *contextRuntime) configChanged(p configChangedPayload) {
	if p.Thinking != nil {
		c.invalidate("thinking_override")
	}
	if c.anchor == nil {
		return
	}
	if p.Provider != nil && *p.Provider != c.anchor.provider {
		c.invalidate("provider_override")
	} else if p.Model != nil && *p.Model != c.anchor.model {
		c.invalidate("model_override")
	}
}

func requestMeasurement(provider llm.Provider, opts []llm.Option, tools []tool.ToolInfo) contextMeasurement {
	// A failing round-context callback can stop a run before a provider is
	// needed (including a deliberately nil provider).
	if provider == nil {
		return contextMeasurement{}
	}
	effective := llm.ApplyOptions(opts)
	return contextMeasurement{
		provider: provider.Name(), model: effective.Model,
		fingerprint: contextFingerprintV1(effective, tools), fingerprintVersion: 1,
	}
}

func (c *contextRuntime) activateMeasurement(m contextMeasurement, usage *llm.Usage, history []llm.Message, sess *persistence) {
	if sess != nil {
		sess.stageMeasurement(m, usage, history)
		return
	}
	c.checkIdentity(m)
	input, output, ok := eligibleUsage(usage)
	if !ok {
		return
	}
	m.prefixMessages, m.inputTokens, m.outputTokens = len(history), input, output
	if c.durable {
		history = measurementHistory(history)
	}
	c.anchor = newUsageAnchor(m, history, &c.generation)
	if c.anchor != nil {
		c.anchor.durable = c.durable
	}
}

func (c *contextRuntime) estimate(history []llm.Message) uint64 {
	estimate := estimateContextTokens(history, c.anchor)
	if c.anchor != nil && c.anchor.generation == nil {
		c.invalidate("estimator_changed")
	}
	return estimate
}

func (c *contextRuntime) overlayFitter(history []llm.Message) overlayFitter {
	if c.overlay == nil || c.overlayGeneration != c.generation || len(c.overlay.history) != len(history) {
		fitter := newOverlayFitter(history)
		c.overlay = &fitter
		c.overlayGeneration = c.generation
	}
	return *c.overlay
}
