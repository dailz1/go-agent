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
	Message    llm.Message `json:"message"`
	ToolCalls  int         `json:"tool_calls"`
	Truncated  bool        `json:"truncated"`
	Usage      llm.Usage   `json:"usage"`
	TotalUsage llm.Usage   `json:"total_usage"`
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
}

// roundCommittedPayload closes a declaration with the round's ordered
// model-visible results: one role=tool message per declared call, in
// declaration order, including soft failures (approval rejection, unknown
// tool, recovered panic) and recovery synthesis.
type roundCommittedPayload struct {
	RunID   string        `json:"run_id"`
	Round   int           `json:"round"`
	Results []llm.Message `json:"results"`
}

// agentCheckpoint is the agent-owned checkpoint payload. The checkpoint is
// a rebuildable accelerator, but the run lifecycle context (whether a run is
// open, its ID, its highest round) and the thread's system prompt would be
// hidden behind it on replay, so they travel inside the snapshot.
type agentCheckpoint struct {
	History   []llm.Message `json:"history"`
	System    string        `json:"system"`
	RunActive bool          `json:"run_active"`
	RunID     string        `json:"run_id,omitempty"`
	LastRound int           `json:"last_round"`
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
	return p, nil
}
