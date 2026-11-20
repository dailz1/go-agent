package agent

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

// openRound is a durable declaration without a commit: the interrupted-round
// state recovery must resolve. results is the round_result confirmed prefix
// in declaration order; slots beyond it have no durable outcome.
type openRound struct {
	runID    string
	round    int
	declared llm.Message
	results  []llm.Message
}

// threadView is the replayed durable state of a thread. systemSet records
// that the frozen system prompt is known — including the legitimate empty
// prompt — so a later run cannot replace it from current configuration.
type threadView struct {
	legacyHistory   bool
	system          string
	systemSet       bool
	runID           string
	history         []llm.Message
	head            int64
	runActive       bool
	cancelled       bool
	lastRound       int
	open            *openRound
	cfg             configState
	queue           []queueEntry
	cfgSeq          int64
	recovery        *recoveryProjection
	proof           []store.Record
	replacements    map[string]store.Record
	episodes        map[string]struct{}
	configRevisions map[int64]struct{}
}

// closeUnknown resolves an interrupted round honestly: the declared
// assistant message joins history, and every declared call gets one
// role=tool IsError result reporting the outcome as unknown. The calls are
// never blindly re-executed; the model decides what to do with the signal.
// The results are returned so the caller can persist them as the round's
// commit, which keeps the log canonical without a second synthesis pass.
func (v *threadView) closeUnknown() []llm.Message {
	results := v.open.resultsWithUnknown()
	v.history = append(v.history, v.open.declared)
	v.history = append(v.history, results...)
	v.open = nil
	return results
}

// resultsWithUnknown merges the recorded round_result prefix with the
// canonical unknown outcomes: recorded positions keep their real result,
// unrecorded positions get unknownResults' canonical form. It returns a
// fresh slice and never mutates the recorded prefix.
func (o *openRound) resultsWithUnknown() []llm.Message {
	all := unknownResults(o.declared)
	copy(all[:len(o.results)], o.results)
	return all
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
		payload.Results = v.open.resultsWithUnknown()
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
	var replacements map[string]store.Record
	var episodes map[string]struct{}
	var configRevisions map[int64]struct{}
	proof := records
	if state.Checkpoint != nil {
		cpState, err = decodeCheckpoint(*state.Checkpoint)
		if err != nil {
			return threadView{}, err
		}
		if cpState != nil && cpState.CodecVersion < store.SchemaV3 {
			// Pre-v3 snapshots carry no control-plane projection; replaying
			// only their tail would silently drop config_changed records
			// hidden behind the checkpoint. Full-log fallback per the D9
			// rule: an old snapshot must never bypass new semantics.
			cpState = nil
		}
		full, ferr := a.store.History(ctx, thread, 0)
		if ferr != nil {
			return threadView{}, fmt.Errorf("agent: rebuild thread %q: %w", thread, ferr)
		}
		// H1: compatibility scans and proof context use exactly Latest's
		// observed prefix. Later control writes belong to absorption.
		for len(full) > 0 && full[len(full)-1].Seq >= state.Head {
			full = full[:len(full)-1]
		}
		proof = full
		cfgSeq := int64(-1)
		for _, r := range full {
			if r.Schema < store.SchemaV1 || r.Schema > store.SchemaV4 {
				return threadView{}, incompatible(r, "unsupported schema in checkpoint prefix")
			}
			if r.Kind == store.KindCheckpoint {
				if err := checkpointVersion(r); err != nil {
					return threadView{}, err
				}
			} else if r.Schema == store.SchemaV4 &&
				r.Kind != store.KindContextRecovery && r.Kind != store.KindHistoryReplaced {
				return threadView{}, incompatible(r, "schema 4 on a legacy record")
			}
			if cpState != nil && cpState.CodecVersion == store.SchemaV3 && r.Schema == store.SchemaV4 {
				cpState = nil // a legacy snapshot must not hide recovery state
			}
			if r.Kind == store.KindConfigChanged && r.Seq < state.Checkpoint.Seq {
				if _, err := decodeConfigChanged(r); err != nil {
					return threadView{}, err
				}
				cfgSeq = r.Seq
				if configRevisions == nil {
					configRevisions = make(map[int64]struct{})
				}
				configRevisions[r.Seq] = struct{}{}
			}
		}
		if cpState != nil && cpState.CodecVersion == store.SchemaV4 {
			prefix := full
			for len(prefix) > 0 && prefix[len(prefix)-1].Seq >= state.Checkpoint.Seq {
				prefix = prefix[:len(prefix)-1]
			}
			canonical, err := replayRecords(threadView{lastRound: -1, cfgSeq: -1}, prefix)
			if err != nil {
				return threadView{}, err
			}
			if !checkpointProjectionMatches(cpState, canonical) {
				cpState = nil
			} else {
				replacements = canonical.replacements
				episodes, configRevisions = canonical.episodes, canonical.configRevisions
			}
		}
		if cpState == nil {
			records = full
		} else {
			base = cpState.History
			if cpState.CodecVersion == store.SchemaV3 {
				cpState.ConfigSeq = &cfgSeq
			}
		}
	}

	v := recoveryProofContext(threadView{head: state.Head, lastRound: -1, history: base, cfgSeq: -1}, proof)
	if cpState != nil {
		v.legacyHistory = cpState.CodecVersion == store.SchemaV3
		// The checkpoint hides the records that established the run
		// lifecycle; the snapshot carries them.
		v.system, v.systemSet, v.runActive, v.runID, v.lastRound =
			cpState.System, true, cpState.RunActive, cpState.RunID, cpState.LastRound
		if cpState.Config != nil {
			v.cfg = *cpState.Config
		}
		if cpState.Queue != nil {
			v.queue = cpState.Queue
		}
		v.cfgSeq, v.recovery = *cpState.ConfigSeq, cpState.Recovery
		v.replacements = replacements
		v.episodes, v.configRevisions = episodes, configRevisions
	}
	v, err = replayRecords(v, records)
	v.head = state.Head
	return v, err
}

// replayRecords applies a finite log prefix, including lifecycle validation.
// Checkpoints are accelerators only; callers supply their already-decoded base.
func replayRecords(v threadView, records []store.Record) (threadView, error) {
	v.history = deepCopyMessages(v.history)
	v.replacements = maps.Clone(v.replacements)
	v.episodes = maps.Clone(v.episodes)
	v.configRevisions = maps.Clone(v.configRevisions)
	if v.proof == nil {
		v = recoveryProofContext(v, records)
	} else if len(records) > 0 && (len(v.proof) == 0 || records[0].Seq > v.proof[len(v.proof)-1].Seq) {
		v.proof = append(append([]store.Record(nil), v.proof...), records...)
	}
	if v.recovery != nil {
		recovery := *v.recovery
		v.recovery = &recovery
	}
	if v.open != nil {
		// Finite-prefix replays (settlement confirmation) must not mutate the
		// caller's projection: copy the open declaration and its recorded
		// results just like the history above.
		open := *v.open
		open.results = deepCopyMessages(open.results)
		v.open = &open
	}
	for i := range records {
		r := records[i]
		if r.Schema < store.SchemaV1 || r.Schema > store.SchemaV4 {
			return v, fmt.Errorf("%w: seq %d schema %d", ErrIncompatibleLog, r.Seq, r.Schema)
		}
		if r.Kind == store.KindCheckpoint {
			if err := checkpointVersion(r); err != nil {
				return v, err
			}
			v.head = r.Seq + 1
			continue // accelerator, never history
		}
		if r.Schema == store.SchemaV4 && r.Kind != store.KindContextRecovery && r.Kind != store.KindHistoryReplaced {
			return v, incompatible(r, "schema 4 on a legacy record")
		}
		switch r.Kind {
		case store.KindContextRecovery:
			if err := v.foldContextRecovery(r, true); err != nil {
				return v, err
			}
			if v.recovery != nil {
				if v.episodes == nil {
					v.episodes = make(map[string]struct{})
				}
				v.episodes[v.recovery.EpisodeID] = struct{}{}
			}
		case store.KindHistoryReplaced:
			if err := v.foldHistoryReplaced(r); err != nil {
				return v, err
			}
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
			v.recovery = nil
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
			if v.recovery != nil {
				if r.ID != recordID(v.runID, "-d"+strconv.Itoa(p.Round)) {
					return v, incompatible(r, "declaration id does not identify the recovery round")
				}
				if err := checkRecoverySuccess(v, r, p.Round); err != nil {
					return v, err
				}
			}
			if p.Kind == terminalTextDeclaration {
				// A FinishRound-Continue keeps the run alive after a
				// tool-free turn: the turn joins history and no open
				// declaration exists (there is nothing to execute).
				if len(toolUseBlocks(p.Message)) > 0 {
					return v, incompatible(r, "terminal_text declaration carries tool calls")
				}
				v.recovery = nil
				v.history = append(v.history, p.Message)
				v.lastRound = p.Round
				v.head = r.Seq + 1
				continue
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
			v.recovery = nil
			v.open = &openRound{runID: p.RunID, round: p.Round, declared: p.Message}
			v.lastRound = p.Round
		case store.KindRoundResult:
			p, err := decodeRoundResult(r)
			if err != nil {
				return v, err
			}
			if v.open == nil {
				return v, incompatible(r, "round result without an open declaration")
			}
			if p.RunID != v.open.runID || p.Round != v.open.round {
				return v, incompatible(r, fmt.Sprintf("round result run/round %s/%d does not match declaration %s/%d",
					p.RunID, p.Round, v.open.runID, v.open.round))
			}
			// The recorded prefix is contiguous by construction (every write
			// goes through the declaration-ordered settle), so the next index
			// must be exactly len(results): this one check rejects gaps,
			// duplicates, reordering, and out-of-range indexes.
			if p.Index != len(v.open.results) {
				return v, incompatible(r, fmt.Sprintf("round result index %d breaks the recorded prefix (next expected %d)",
					p.Index, len(v.open.results)))
			}
			calls := toolUseBlocks(v.open.declared)
			if p.Index >= len(calls) {
				return v, incompatible(r, fmt.Sprintf("round result index %d exceeds the declared calls (%d)", p.Index, len(calls)))
			}
			blocks := toolResultBlocks(p.Result)
			if len(blocks) != 1 || blocks[0].ToolUseID != calls[p.Index].ID {
				return v, incompatible(r, fmt.Sprintf("round result %d does not pair with declared call %q", p.Index, calls[p.Index].ID))
			}
			if want := recordID(p.RunID, "-r"+strconv.Itoa(p.Round)+"-"+strconv.Itoa(p.Index)); r.ID != want {
				return v, incompatible(r, fmt.Sprintf("round_result record id %q does not match its payload (want %q)", r.ID, want))
			}
			v.open.results = append(v.open.results, p.Result)
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
			// A batch closing a round with recorded results must match them
			// position for position: the batch and the records are two views
			// of the same outcomes, never two versions of the truth. Both
			// sides are canonicalized first: empty slices round-trip through
			// omitempty as nil, which must not read as a changed result.
			for i, saved := range v.open.results {
				if !reflect.DeepEqual(canonicalResultMessage(saved), canonicalResultMessage(p.Results[i])) {
					return v, incompatible(r, fmt.Sprintf("committed batch alters recorded result %d", i))
				}
			}
			v.history = append(v.history, v.open.declared)
			v.history = append(v.history, p.Results...)
			v.open = nil
		case store.KindConfigChanged:
			// Valid anywhere: a host may retune a thread between runs or
			// while one is active. It carries no lifecycle constraints.
			p, err := decodeConfigChanged(r)
			if err != nil {
				return v, err
			}
			v.cfg.fold(p)
			v.cfgSeq = r.Seq
			if v.configRevisions == nil {
				v.configRevisions = make(map[int64]struct{})
			}
			v.configRevisions[r.Seq] = struct{}{}
		case store.KindInputQueued:
			p, err := decodeInputQueued(r)
			if err != nil {
				return v, err
			}
			v.queue = append(v.queue, queueEntry{ID: r.ID, Input: p.Input})
		case store.KindInputConsumed:
			p, err := decodeInputConsumed(r)
			if err != nil {
				return v, err
			}
			if !v.runActive || v.open != nil || v.recovery != nil {
				// A legal D1 drain runs between a committed round and the next
				// declaration: never mid-declaration, never outside a run.
				return v, incompatible(r, "input consumed outside the drain window")
			}
			if len(p.Inputs) > len(v.queue) {
				return v, incompatible(r, "consumed inputs exceed the pending queue")
			}
			for i, e := range p.Inputs {
				if v.queue[i].ID != e.ID || v.queue[i].Input != e.Input {
					return v, incompatible(r, fmt.Sprintf("consumed input %d does not match the queue prefix (id %q)", i, e.ID))
				}
				v.history = append(v.history, llm.UserMessage(e.Input))
			}
			v.queue = v.queue[len(p.Inputs):]
		case store.KindRunClosed:
			p, err := decodeRunClosed(r)
			if err != nil {
				return v, err
			}
			if !v.runActive || v.open != nil {
				return v, incompatible(r, "run_closed outside the cooperative-end window")
			}
			if p.RunID != v.runID {
				return v, incompatible(r, fmt.Sprintf("run_closed for run %q does not match active run %q", p.RunID, v.runID))
			}
			if v.recovery != nil {
				return v, incompatible(r, "run_closed before a successful recovery declaration")
			}
			v.runActive = false
			v.cancelled = false
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
			if v.recovery != nil {
				if p.Truncated || r.ID != recordID(v.runID, "-done") {
					return v, incompatible(r, "done does not identify a successful recovery reply")
				}
				if err := checkRecoverySuccess(v, r, v.lastRound+1); err != nil {
					return v, err
				}
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
			v.recovery = nil
		case store.KindError:
			// Terminal failure marker kept for compatibility; agent v1 never
			// writes it. Its payload has no semantics for this build, but the
			// record closes the active run — including any open declaration,
			// whose outcome stays unknown exactly like an interruption.
			// Legacy KindError is the only identity-less compatibility
			// exception that may clear an active recovery.
			if !v.runActive {
				return v, incompatible(r, "error record without an active run")
			}
			if v.open != nil && len(v.open.results) > 0 {
				// Recorded results are real outcomes and must not disappear:
				// complete the declaration/results history via the merge rule,
				// then close. The legacy no-records behavior is kept verbatim.
				// The resulting terminal state still refuses Resume.
				results := v.open.resultsWithUnknown()
				v.history = append(v.history, v.open.declared)
				v.history = append(v.history, results...)
			}
			v.open = nil
			v.runActive = false
			v.cancelled = false
			v.recovery = nil
		default:
			return v, fmt.Errorf("%w: unknown record kind %q at seq %d", ErrIncompatibleLog, r.Kind, r.Seq)
		}
		v.head = r.Seq + 1
	}
	return v, nil
}

// All V4 snapshots, including proactive compaction checkpoints, must agree
// with the canonical prefix before any of their state can be adopted.
func checkpointProjectionMatches(cp *agentCheckpoint, v threadView) bool {
	var cfg configState
	if cp.Config != nil {
		cfg = *cp.Config
	}
	return v.systemSet && v.open == nil && !v.cancelled &&
		cp.System == v.system && cp.RunActive == v.runActive &&
		cp.RunID == v.runID && cp.LastRound == v.lastRound &&
		cp.ConfigSeq != nil && *cp.ConfigSeq == v.cfgSeq &&
		reflect.DeepEqual(cp.History, seededHistory(v.system, v.history)) &&
		reflect.DeepEqual(cfg, v.cfg) && slices.Equal(cp.Queue, v.queue) &&
		reflect.DeepEqual(cp.Recovery, v.recovery)
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
