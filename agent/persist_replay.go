package agent

import (
	"context"
	"fmt"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

// openRound is a durable declaration without a commit: the interrupted-round
// state recovery must resolve.
type openRound struct {
	runID    string
	round    int
	declared llm.Message
}

// threadView is the replayed durable state of a thread. systemSet records
// that the frozen system prompt is known — including the legitimate empty
// prompt — so a later run cannot replace it from current configuration.
type threadView struct {
	system    string
	systemSet bool
	runID     string
	history   []llm.Message
	head      int64
	runActive bool
	cancelled bool
	lastRound int
	open      *openRound
}

// closeUnknown resolves an interrupted round honestly: the declared
// assistant message joins history, and every declared call gets one
// role=tool IsError result reporting the outcome as unknown. The calls are
// never blindly re-executed; the model decides what to do with the signal.
// The results are returned so the caller can persist them as the round's
// commit, which keeps the log canonical without a second synthesis pass.
func (v *threadView) closeUnknown() []llm.Message {
	results := unknownResults(v.open.declared)
	v.history = append(v.history, v.open.declared)
	v.history = append(v.history, results...)
	v.open = nil
	return results
}

func unknownResults(declared llm.Message) []llm.Message {
	calls := toolUseBlocks(declared)
	results := make([]llm.Message, 0, len(calls))
	for _, call := range calls {
		results = append(results, llm.ToolResultMessage(call.ID,
			tool.NewErrorResult("outcome unknown: the run was interrupted before this tool call's result could be recorded")))
	}
	return results
}

// cancelRecord builds one atomic terminal record without changing the view.
func (v threadView) cancelRecord() (store.Record, error) {
	payload := runCancelledPayload{RunID: v.runID, Results: []llm.Message{}}
	if v.open != nil {
		round := v.open.round
		payload.OpenRound = &round
		payload.Results = unknownResults(v.open.declared)
	}
	r, err := encodeRecord(store.KindRunCancelled, recordID(v.runID, "-cancel"), payload)
	r.Schema = store.SchemaV2
	return r, err
}

// incompatible wraps ErrIncompatibleLog with the record's position.
func incompatible(r store.Record, why string) error {
	return fmt.Errorf("%w: seq %d: %s", ErrIncompatibleLog, r.Seq, why)
}

// replayThread rebuilds the durable state of a thread. The checkpoint is a
// rebuildable accelerator: when it cannot be decoded into a structurally
// valid snapshot, it is discarded and the full log is read through
// Store.History — the fallback Latest cannot serve, because it omits
// everything before the checkpoint. Lifecycle records are validated against
// each other: a log whose records contradict the run/round state machine is
// incompatible, not best-effort interpretable.
func (a *Agent) replayThread(ctx context.Context, thread string) (threadView, error) {
	state, err := a.store.Latest(ctx, thread)
	if err != nil {
		return threadView{}, fmt.Errorf("agent: read thread %q: %w", thread, err)
	}
	records := state.Tail
	var base []llm.Message
	var cpState *agentCheckpoint
	if state.Checkpoint != nil {
		cpState, err = decodeCheckpoint(*state.Checkpoint)
		if err != nil {
			return threadView{}, err
		}
		if cpState != nil {
			base = cpState.History
		}
		if cpState == nil {
			// Checkpoint unusable: full-log fallback.
			full, ferr := a.store.History(ctx, thread, 0)
			if ferr != nil {
				return threadView{}, fmt.Errorf("agent: rebuild thread %q: %w", thread, ferr)
			}
			records = full
		}
	}

	v := threadView{head: state.Head, lastRound: -1, history: base}
	if cpState != nil {
		// The checkpoint hides the records that established the run
		// lifecycle; the snapshot carries them.
		v.system, v.systemSet, v.runActive, v.runID, v.lastRound =
			cpState.System, true, cpState.RunActive, cpState.RunID, cpState.LastRound
	}
	v, err = replayRecords(v, records)
	v.head = state.Head
	return v, err
}

// replayRecords applies a finite log prefix, including lifecycle validation.
// Checkpoints are accelerators only; callers supply their already-decoded base.
func replayRecords(v threadView, records []store.Record) (threadView, error) {
	v.history = deepCopyMessages(v.history)
	for i := range records {
		r := records[i]
		if r.Schema < store.SchemaV1 || r.Schema > store.SchemaV2 {
			return v, fmt.Errorf("%w: seq %d schema %d", ErrIncompatibleLog, r.Seq, r.Schema)
		}
		if r.Kind == store.KindCheckpoint {
			if err := checkpointVersion(r); err != nil {
				return v, err
			}
			v.head = r.Seq + 1
			continue // accelerator, never history
		}
		switch r.Kind {
		case store.KindRunStarted:
			p, err := decodeRunStarted(r)
			if err != nil {
				return v, err
			}
			if v.runActive {
				return v, incompatible(r, "run_started while a run is active")
			}
			if !v.systemSet {
				v.system, v.systemSet = p.SystemPrompt, true
			}
			v.runID, v.runActive, v.lastRound = p.RunID, true, -1
			v.cancelled = false
			v.history = append(v.history, llm.UserMessage(p.Input))
		case store.KindRoundDeclared:
			p, err := decodeRoundDeclared(r)
			if err != nil {
				return v, err
			}
			if !v.runActive {
				return v, incompatible(r, "declaration without an active run")
			}
			if v.open != nil {
				return v, incompatible(r, "declaration while a declaration is open")
			}
			if p.RunID != v.runID || p.Round != v.lastRound+1 {
				return v, incompatible(r, fmt.Sprintf("declaration run/round %s/%d breaks the chain at run %s round %d",
					p.RunID, p.Round, v.runID, v.lastRound+1))
			}
			if p.Message.Role != llm.RoleAssistant {
				return v, incompatible(r, "declaration message must be an assistant message")
			}
			calls := toolUseBlocks(p.Message)
			if len(calls) == 0 {
				return v, incompatible(r, "declaration without tool calls")
			}
			seen := make(map[string]struct{}, len(calls))
			for _, call := range calls {
				if call.ID == "" {
					return v, incompatible(r, "declaration with an empty tool-call ID")
				}
				if _, dup := seen[call.ID]; dup {
					return v, incompatible(r, "declaration with duplicate tool-call ID "+call.ID)
				}
				seen[call.ID] = struct{}{}
			}
			v.open = &openRound{runID: p.RunID, round: p.Round, declared: p.Message}
			v.lastRound = p.Round
		case store.KindRoundCommitted:
			p, err := decodeRoundCommitted(r)
			if err != nil {
				return v, err
			}
			if v.open == nil {
				return v, incompatible(r, "commit without an open declaration")
			}
			if p.RunID != v.open.runID || p.Round != v.open.round {
				return v, incompatible(r, fmt.Sprintf("commit run/round %s/%d does not close declaration %s/%d",
					p.RunID, p.Round, v.open.runID, v.open.round))
			}
			if err := validateResultBatch(v.open.declared, p.Results); err != nil {
				return v, incompatible(r, err.Error())
			}
			v.history = append(v.history, v.open.declared)
			v.history = append(v.history, p.Results...)
			v.open = nil
		case store.KindRunCancelled:
			if err := v.applyCancellation(r); err != nil {
				return v, err
			}
		case store.KindAgentEvent:
			p, err := decodeAgentEvent(r)
			if err != nil {
				return v, err
			}
			if !v.runActive {
				return v, incompatible(r, "done without a run")
			}
			if v.open != nil {
				return v, incompatible(r, "done with a declaration still open")
			}
			if p.Message.Role != llm.RoleAssistant {
				return v, incompatible(r, "done message must be an assistant message")
			}
			if p.Truncated {
				// The declared assistant message is already in history.
				if len(p.Message.Content) > 0 {
					return v, incompatible(r, "truncated done re-carries the declared assistant message")
				}
			} else {
				// A terminal reply comes from the no-tool branch of the loop;
				// tool calls in it would replay as an unpaired batch.
				if len(toolUseBlocks(p.Message)) > 0 {
					return v, incompatible(r, "non-truncated done carries tool calls")
				}
				// An empty assistant turn is a legitimate terminal result and
				// is preserved as a turn.
				v.history = append(v.history, p.Message)
			}
			v.runActive = false
			v.cancelled = false
		case store.KindError:
			// Terminal failure marker kept for compatibility; agent v1 never
			// writes it. Its payload has no semantics for this build, but the
			// record closes the active run — including any open declaration,
			// whose outcome stays unknown exactly like an interruption.
			if !v.runActive {
				return v, incompatible(r, "error record without an active run")
			}
			v.open = nil
			v.runActive = false
			v.cancelled = false
		default:
			return v, fmt.Errorf("%w: unknown record kind %q at seq %d", ErrIncompatibleLog, r.Kind, r.Seq)
		}
		v.head = r.Seq + 1
	}
	return v, nil
}

// checkpointSystemMatches verifies that the snapshot's entire system prefix
// is exactly the frozen prompt: no prefix messages at all for an empty
// prompt, or exactly one system message holding a single text block with the
// persisted text. Anything else — extra system messages, extra or non-text
// blocks, different text — marks the snapshot corrupt even though it
// decodes, and replay falls back to the full log.
func checkpointSystemMatches(cp *agentCheckpoint) bool {
	prefix := 0
	for prefix < len(cp.History) && cp.History[prefix].Role == llm.RoleSystem {
		prefix++
	}
	if cp.System == "" {
		return prefix == 0
	}
	if prefix != 1 || len(cp.History[0].Content) != 1 {
		return false
	}
	tb, ok := cp.History[0].Content[0].(llm.TextBlock)
	return ok && tb.Text == cp.System
}

// validateResultBatch checks that a commit's results pair one-to-one, in
// order, with the declared tool calls; anything else would replay into a
// provider-invalid history.
func validateResultBatch(declared llm.Message, results []llm.Message) error {
	calls := toolUseBlocks(declared)
	if len(results) != len(calls) {
		return fmt.Errorf("%d results for %d declared calls", len(results), len(calls))
	}
	for i, res := range results {
		blocks := toolResultBlocks(res)
		if res.Role != llm.RoleTool || len(blocks) != 1 || blocks[0].ToolUseID != calls[i].ID {
			return fmt.Errorf("result %d does not pair with declared call %q", i, calls[i].ID)
		}
	}
	return nil
}

// toolResultBlocks extracts the tool result blocks of a message.
func toolResultBlocks(m llm.Message) []llm.ToolResultBlock {
	var out []llm.ToolResultBlock
	for _, blk := range m.Content {
		if rb, ok := blk.(llm.ToolResultBlock); ok {
			out = append(out, rb)
		}
	}
	return out
}
