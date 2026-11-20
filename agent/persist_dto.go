package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
)

// ErrIncompatibleLog is returned when a persisted thread log cannot be
// replayed by this binary: an unknown record kind, an unknown event type, an
// envelope version this build does not understand, a payload that does not
// decode, or lifecycle records that contradict each other. The log is never
// silently ignored or partially interpreted.
var ErrIncompatibleLog = errors.New("agent: persisted thread log is not compatible with this build")

// eventEnvelopeV1 is the envelope schema version written by this build. A
// record with any other envelope version is rejected on replay.
const eventEnvelopeV1 = 1

// eventEnvelope wraps a persisted AgentEvent payload with an explicit
// discriminator and version. The sealed AgentEvent interface has no exported
// discriminator and some variants embed errors, so events are persisted
// through these versioned DTOs instead of direct interface marshaling.
type eventEnvelope struct {
	Type    string          `json:"type"`
	Version int             `json:"version"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// doneEventPayload is the durable subset of DoneEvent: everything replay
// needs to restore the thread and close the run. Message is empty for a
// truncated run: the tool-calling assistant message is already in history
// through its declaration and must not be appended twice. Retries are
// per-invocation telemetry (their errors are not persistable) and stay out
// of the log.
type doneEventPayload struct {
	Message            llm.Message      `json:"message"`
	ToolCalls          int              `json:"tool_calls"`
	Truncated          bool             `json:"truncated"`
	Usage              llm.Usage        `json:"usage"`
	TotalUsage         llm.Usage        `json:"total_usage"`
	ContextMeasurement *doneMeasurement `json:"context_measurement"`
}

// runStartedPayload opens a run: the exact input and the thread's system
// prompt (possibly empty — an empty prompt is frozen like any other).
// Replay always uses the persisted prompt, so changing WithSystemPrompt
// between processes cannot silently rewrite historical model context.
type runStartedPayload struct {
	RunID        string `json:"run_id"`
	Input        string `json:"input"`
	SystemPrompt string `json:"system_prompt"`
}

// roundDeclaredPayload is the atomic pre-execution declaration: the round's
// complete assistant message (text/reasoning plus every ordered tool call).
// It is durable before any tool runs and before the approval callback.
type roundDeclaredPayload struct {
	RunID   string      `json:"run_id"`
	Round   int         `json:"round"`
	Message llm.Message `json:"message"`
	// Kind generalizes the declaration: "" is the legacy tool-round form
	// (tool calls must follow), "terminal_text" declares a tool-free
	// assistant turn persisted for a FinishRound-Continue decision.
	Kind               string               `json:"kind,omitempty"`
	ContextMeasurement *declaredMeasurement `json:"context_measurement,omitempty"`
}

// terminalTextDeclaration marks a tool-free declaration persisted for a
// FinishRound-Continue decision.
const terminalTextDeclaration = "terminal_text"

// roundCommittedPayload closes a declaration with the round's ordered
// model-visible results: one role=tool message per declared call, in
// declaration order, including soft failures (approval rejection, unknown
// tool, recovered panic) and recovery synthesis.
type roundCommittedPayload struct {
	RunID   string        `json:"run_id"`
	Round   int           `json:"round"`
	Results []llm.Message `json:"results"`
}

type runCancelledPayload struct {
	RunID     string        `json:"run_id"`
	OpenRound *int          `json:"open_round"`
	Results   []llm.Message `json:"results"`
}

func decodeRunCancelled(r store.Record) (runCancelledPayload, error) {
	// Decode concrete wire blocks here: llm.Message's permissive decoder
	// intentionally ignores unknown fields for provider compatibility.
	var wire struct {
		RunID     string          `json:"run_id"`
		OpenRound json.RawMessage `json:"open_round"`
		Results   []struct {
			Role    llm.Role              `json:"role"`
			Content []llm.ToolResultBlock `json:"content"`
		} `json:"results"`
	}
	if err := strictDecode(r.Payload, &wire); err != nil {
		return runCancelledPayload{}, incompatible(r, err.Error())
	}
	if r.Schema != store.SchemaV2 || wire.RunID == "" || len(wire.OpenRound) == 0 || wire.Results == nil {
		return runCancelledPayload{}, incompatible(r, "cancellation requires schema 2 and run_id/open_round/results")
	}
	p := runCancelledPayload{RunID: wire.RunID, Results: make([]llm.Message, 0, len(wire.Results))}
	if err := json.Unmarshal(wire.OpenRound, &p.OpenRound); err != nil {
		return runCancelledPayload{}, incompatible(r, "invalid cancellation round")
	}
	for _, res := range wire.Results {
		if res.Role != llm.RoleTool || len(res.Content) != 1 {
			return runCancelledPayload{}, incompatible(r, "cancellation results must be single tool-result messages")
		}
		p.Results = append(p.Results, llm.Message{Role: res.Role, Content: []llm.ContentBlock{res.Content[0]}})
	}
	return p, nil
}

// configChangedPayload is the runtime config override carried by one
// config_changed record. Nil means "unchanged"; non-nil overwrites the
// field for every subsequent provider request in the thread. Provider is
// omitted when nil so records without a switch stay byte-identical to the
// pre-provider payload (older binaries fail loudly on the field via
// strict-decode instead of misreading it).
type configChangedPayload struct {
	Model    *string `json:"model"`
	Thinking *string `json:"thinking"`
	Provider *string `json:"provider,omitempty"`
}

func decodeConfigChanged(r store.Record) (configChangedPayload, error) {
	if r.Schema != store.SchemaV2 {
		return configChangedPayload{}, incompatible(r, fmt.Sprintf("config_changed schema %d", r.Schema))
	}
	var p configChangedPayload
	if err := strictDecode(r.Payload, &p); err != nil {
		return configChangedPayload{}, incompatible(r, "invalid config_changed payload")
	}
	// The pair rule and the non-empty name, mirrored from the write side:
	// a conforming writer never produces these shapes, so seeing one means
	// a malformed or hand-edited log — fail closed, never guess.
	if p.Provider != nil {
		if *p.Provider == "" {
			return configChangedPayload{}, incompatible(r, "config_changed provider name is empty")
		}
		if p.Model == nil || *p.Model == "" {
			return configChangedPayload{}, incompatible(r, "config_changed provider switch without a model")
		}
	}
	return p, nil
}

func decodeInputQueued(r store.Record) (inputQueuedPayload, error) {
	if r.Schema != store.SchemaV2 {
		return inputQueuedPayload{}, incompatible(r, fmt.Sprintf("input_queued schema %d", r.Schema))
	}
	var p inputQueuedPayload
	if err := strictDecode(r.Payload, &p); err != nil {
		return inputQueuedPayload{}, incompatible(r, "invalid input_queued payload")
	}
	return p, nil
}

func decodeInputConsumed(r store.Record) (inputConsumedPayload, error) {
	if r.Schema != store.SchemaV2 {
		return inputConsumedPayload{}, incompatible(r, fmt.Sprintf("input_consumed schema %d", r.Schema))
	}
	var p inputConsumedPayload
	if err := strictDecode(r.Payload, &p); err != nil {
		return inputConsumedPayload{}, incompatible(r, "invalid input_consumed payload")
	}
	for _, e := range p.Inputs {
		if e.ID == "" || e.Input == "" {
			return inputConsumedPayload{}, incompatible(r, "consumed input entry must carry id and input")
		}
	}
	return p, nil
}

// configState is the folded effective config of a thread. It starts from the
// Agent's WithLLMOptions defaults and is overridden field-by-field by each
// config_changed record in replay order (last write wins). Provider is
// folded the same way: once a record names a provider, every later round
// resolves it from the Agent's injected provider table.
type configState struct {
	Model    *string `json:"model,omitempty"`
	Thinking *string `json:"thinking,omitempty"`
	Provider *string `json:"provider,omitempty"`
}

func (c *configState) fold(p configChangedPayload) {
	if p.Model != nil {
		c.Model = p.Model
	}
	if p.Thinking != nil {
		c.Thinking = p.Thinking
	}
	if p.Provider != nil {
		c.Provider = p.Provider
	}
}

// apply returns a fresh option slice: the base options with the effective
// overrides appended after them, so the override wins field-by-field.
func (c configState) apply(base []llm.Option) []llm.Option {
	out := make([]llm.Option, 0, len(base)+2)
	out = append(out, base...)
	if c.Model != nil {
		out = append(out, llm.WithModel(*c.Model))
	}
	if c.Thinking != nil {
		out = append(out, llm.WithThinking(*c.Thinking))
	}
	return out
}

// agentCheckpoint is the agent-owned checkpoint payload. The checkpoint is
// a rebuildable accelerator, but the run lifecycle context (whether a run is
// open, its ID, its highest round) and the thread's system prompt would be
// hidden behind it on replay, so they travel inside the snapshot.
type agentCheckpoint struct {
	CodecVersion  int                      `json:"codec_version,omitempty"`
	History       []llm.Message            `json:"history"`
	System        string                   `json:"system"`
	RunActive     bool                     `json:"run_active"`
	RunID         string                   `json:"run_id,omitempty"`
	LastRound     int                      `json:"last_round"`
	Config        *configState             `json:"config,omitempty"`
	Queue         []queueEntry             `json:"queue,omitempty"`
	ConfigSeq     *int64                   `json:"config_seq,omitempty"`
	Recovery      *recoveryProjection      `json:"recovery"`
	ContextAnchor *contextAnchorProjection `json:"context_anchor"`
}

// checkpointVersion is checked even when the snapshot is not used. Unknown
// versions must not disappear behind structural fallback to the full log.
func checkpointVersion(r store.Record) error {
	if r.Schema < store.SchemaV1 || r.Schema > store.SchemaV5 {
		return incompatible(r, fmt.Sprintf("checkpoint schema %d", r.Schema))
	}
	var version struct {
		CodecVersion int `json:"codec_version"`
	}
	if err := json.Unmarshal(r.Payload, &version); err == nil {
		if version.CodecVersion > store.SchemaV5 {
			return incompatible(r, fmt.Sprintf("checkpoint codec version %d", version.CodecVersion))
		}
	}
	return nil
}

func decodeCheckpoint(r store.Record) (*agentCheckpoint, error) {
	if err := checkpointVersion(r); err != nil {
		return nil, err
	}
	var cp agentCheckpoint
	if err := strictDecode(r.Payload, &cp); err != nil {
		return nil, nil // supported but corrupt snapshot: rebuild the log
	}
	if (r.Schema == store.SchemaV1 && cp.CodecVersion != 0) ||
		(r.Schema == store.SchemaV2 && cp.CodecVersion != store.SchemaV2) ||
		(r.Schema == store.SchemaV3 && cp.CodecVersion != store.SchemaV3) ||
		(r.Schema == store.SchemaV4 && cp.CodecVersion != store.SchemaV4) ||
		(r.Schema == store.SchemaV5 && cp.CodecVersion != store.SchemaV5) {
		return nil, nil
	}
	if r.Schema < store.SchemaV4 && (cp.ConfigSeq != nil || cp.Recovery != nil) {
		return nil, nil
	}
	if r.Schema < store.SchemaV5 && cp.ContextAnchor != nil {
		return nil, nil
	}
	if !cp.RunActive || cp.RunID == "" || cp.LastRound < -1 || cp.History == nil {
		return nil, nil
	}
	if _, err := partitionHistory(cp.History); err != nil || !checkpointSystemMatches(&cp) {
		return nil, nil
	}
	if r.Schema >= store.SchemaV4 {
		var presence struct {
			Recovery json.RawMessage `json:"recovery"`
		}
		if err := json.Unmarshal(r.Payload, &presence); err != nil ||
			len(presence.Recovery) == 0 || cp.ConfigSeq == nil || *cp.ConfigSeq < -1 {
			return nil, nil
		}
		if cp.Recovery != nil {
			rp, err := decodeRecoveryProjection(presence.Recovery)
			if err != nil || rp.RunID != cp.RunID || rp.Round != cp.LastRound+1 ||
				rp.ConfigSeq > *cp.ConfigSeq {
				return nil, nil
			}
			if rp.Phase == "ready" || rp.Phase == "dispatched" {
				size := estimateRunes(cp.History)
				if size > rp.BudgetRunes || size >= rp.BeforeRunes {
					return nil, nil
				}
			}
			cp.Recovery = rp
		}
	}
	if r.Schema == store.SchemaV5 && !validCheckpointAnchor(r.Payload, cp.ContextAnchor) {
		return nil, nil
	}
	return &cp, nil
}

// recordID derives a stable record ID. Records are generated exactly once
// per logical write, so an ambiguous-failure retry reproduces byte-identical
// caller content, which the Store's idempotent retry requires.
func recordID(runID, suffix string) string { return runID + suffix }

// encodeRecord builds a Record with a strict-JSON payload.
func encodeRecord(kind, id string, payload any) (store.Record, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return store.Record{}, fmt.Errorf("agent: encode %s payload: %w", kind, err)
	}
	return store.Record{Kind: kind, Schema: store.SchemaV1, ID: id, Payload: data}, nil
}

// strictDecode decodes payload into v, rejecting unknown fields: a payload
// this build does not fully understand must fail loudly, not silently drop
// data.
func strictDecode(payload json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data after payload object")
	}
	return nil
}

// decodeRunStarted strictly decodes a run_started payload.
func decodeRunStarted(r store.Record) (runStartedPayload, error) {
	var p runStartedPayload
	if err := strictDecode(r.Payload, &p); err != nil {
		return runStartedPayload{}, fmt.Errorf("%w: run_started seq %d: %v", ErrIncompatibleLog, r.Seq, err)
	}
	return p, nil
}

// decodeRoundDeclared strictly decodes a round_declared payload.
func decodeRoundDeclared(r store.Record) (roundDeclaredPayload, error) {
	var p roundDeclaredPayload
	if err := strictDecode(r.Payload, &p); err != nil {
		return roundDeclaredPayload{}, fmt.Errorf("%w: round_declared seq %d: %v", ErrIncompatibleLog, r.Seq, err)
	}
	if p.Kind != "" && p.Kind != terminalTextDeclaration {
		return roundDeclaredPayload{}, fmt.Errorf("%w: round_declared seq %d: unknown declaration kind %q",
			ErrIncompatibleLog, r.Seq, p.Kind)
	}
	if err := validateDeclaredMeasurement(r, p.ContextMeasurement); err != nil {
		return roundDeclaredPayload{}, err
	}
	return p, nil
}

// runClosedPayload cooperatively closes a run at a FinishRound-End: the
// declared assistant message and results are already in history, so no
// message is re-carried. Replay derives the terminal view from the log.
type runClosedPayload struct {
	RunID      string    `json:"run_id"`
	ToolCalls  int       `json:"tool_calls"`
	Usage      llm.Usage `json:"usage"`
	TotalUsage llm.Usage `json:"total_usage"`
}

func decodeRunClosed(r store.Record) (runClosedPayload, error) {
	if r.Schema != store.SchemaV2 {
		return runClosedPayload{}, fmt.Errorf("%w: run_closed schema %d", ErrIncompatibleLog, r.Schema)
	}
	var p runClosedPayload
	if err := strictDecode(r.Payload, &p); err != nil {
		return runClosedPayload{}, fmt.Errorf("%w: run_closed seq %d: %v", ErrIncompatibleLog, r.Seq, err)
	}
	if p.RunID == "" {
		return runClosedPayload{}, fmt.Errorf("%w: run_closed without a run id", ErrIncompatibleLog)
	}
	return p, nil
}

// decodeRoundCommitted strictly decodes a round_committed payload.
func decodeRoundCommitted(r store.Record) (roundCommittedPayload, error) {
	var p roundCommittedPayload
	if err := strictDecode(r.Payload, &p); err != nil {
		return roundCommittedPayload{}, fmt.Errorf("%w: round_committed seq %d: %v", ErrIncompatibleLog, r.Seq, err)
	}
	return p, nil
}

// roundResultPayload durably records one declared call's result while the
// round is still open: the per-call durability unit that makes every
// delivered ToolResultEvent replayable before round_committed lands.
type roundResultPayload struct {
	RunID  string      `json:"run_id"`
	Round  int         `json:"round"`
	Index  int         `json:"index"`
	Result llm.Message `json:"result"`
}

// decodeRoundResult strictly decodes a round_result payload. The wire shape
// mirrors decodeRunCancelled: concrete tool-result blocks, pointer ints to
// distinguish a missing round/index from a legitimate zero.
func decodeRoundResult(r store.Record) (roundResultPayload, error) {
	var wire struct {
		RunID  string `json:"run_id"`
		Round  *int   `json:"round"`
		Index  *int   `json:"index"`
		Result *struct {
			Role    llm.Role              `json:"role"`
			Content []llm.ToolResultBlock `json:"content"`
		} `json:"result"`
	}
	if err := strictDecode(r.Payload, &wire); err != nil {
		return roundResultPayload{}, fmt.Errorf("%w: round_result seq %d: %v", ErrIncompatibleLog, r.Seq, err)
	}
	if r.Schema != store.SchemaV1 || wire.RunID == "" || wire.Round == nil || wire.Index == nil || wire.Result == nil {
		return roundResultPayload{}, fmt.Errorf("%w: round_result seq %d: requires schema 1 and run_id/round/index/result", ErrIncompatibleLog, r.Seq)
	}
	if *wire.Round < 0 || *wire.Index < 0 {
		return roundResultPayload{}, fmt.Errorf("%w: round_result seq %d: negative round/index", ErrIncompatibleLog, r.Seq)
	}
	if wire.Result.Role != llm.RoleTool || len(wire.Result.Content) != 1 {
		return roundResultPayload{}, fmt.Errorf("%w: round_result seq %d: must be a single tool-result message", ErrIncompatibleLog, r.Seq)
	}
	if wire.Result.Content[0].Type != "tool_result" {
		return roundResultPayload{}, fmt.Errorf("%w: round_result seq %d: block type must be tool_result", ErrIncompatibleLog, r.Seq)
	}
	return roundResultPayload{
		RunID:  wire.RunID,
		Round:  *wire.Round,
		Index:  *wire.Index,
		Result: llm.Message{Role: wire.Result.Role, Content: []llm.ContentBlock{canonicalResultBlock(wire.Result.Content[0])}},
	}, nil
}

// canonicalResultBlock normalizes a decoded tool-result block so marshal and
// decode agree across round trips: encoding/json drops empty slices through
// omitempty, so a decoded non-nil empty Images slice or empty image Data
// would fail the recorded-vs-batch DeepEqual on the next replay.
func canonicalResultBlock(b llm.ToolResultBlock) llm.ToolResultBlock {
	if len(b.Images) == 0 {
		b.Images = nil
	}
	for i, img := range b.Images {
		if len(img.Data) == 0 {
			img.Data = nil
		}
		b.Images[i] = img
	}
	return b
}

func canonicalResultMessage(m llm.Message) llm.Message {
	out := m
	for i, block := range out.Content {
		if rb, ok := block.(llm.ToolResultBlock); ok {
			out.Content[i] = canonicalResultBlock(rb)
		}
	}
	return out
}

// decodeAgentEvent strictly decodes an agent_event envelope. Only the done
// event exists in v1; unknown types and any envelope version other than the
// current one are compatibility errors, never silently ignored.
func decodeAgentEvent(r store.Record) (doneEventPayload, error) {
	var env eventEnvelope
	if err := strictDecode(r.Payload, &env); err != nil {
		return doneEventPayload{}, fmt.Errorf("%w: agent_event seq %d: %v", ErrIncompatibleLog, r.Seq, err)
	}
	if env.Version != eventEnvelopeV1 {
		return doneEventPayload{}, fmt.Errorf("%w: agent_event seq %d: envelope version %d", ErrIncompatibleLog, r.Seq, env.Version)
	}
	if env.Type != "done" {
		return doneEventPayload{}, fmt.Errorf("%w: agent_event seq %d: unknown event type %q", ErrIncompatibleLog, r.Seq, env.Type)
	}
	var p doneEventPayload
	if err := strictDecode(env.Payload, &p); err != nil {
		return doneEventPayload{}, fmt.Errorf("%w: agent_event seq %d: %v", ErrIncompatibleLog, r.Seq, err)
	}
	if err := validateDoneMeasurement(r, env.Payload, p); err != nil {
		return doneEventPayload{}, err
	}
	return p, nil
}
