package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
)

// checkpoint persists a compaction snapshot so replay never re-runs a
// nondeterministic summarizer. The snapshot carries the run lifecycle
// context the tail replay needs. The ID is bound to the record's future
// position, keeping retries byte-identical.
func (p *persistence) checkpoint(ctx context.Context, history []llm.Message) error {
	payload, err := json.Marshal(agentCheckpoint{
		History:   history,
		System:    p.system,
		RunActive: true,
		RunID:     p.runID,
		LastRound: p.lastRound,
	})
	if err != nil {
		return fmt.Errorf("agent: encode checkpoint: %w", err)
	}
	return p.append(ctx, store.Record{
		Kind: store.KindCheckpoint, Schema: store.SchemaV1,
		ID:      fmt.Sprintf("cp-%d", p.head),
		Payload: payload,
	})
}

// release gives up thread ownership. Safe to call once per session.
func (p *persistence) release() { releaseThread(p.a.store, p.thread) }

// seededHistory prepends the thread's frozen system message to the replayed
// history — unless the history already begins with it, which is the case for
// a checkpoint snapshot whose compacted view carries the prefix.
func seededHistory(system string, history []llm.Message) []llm.Message {
	if system == "" {
		return history
	}
	if len(history) > 0 && history[0].Role == llm.RoleSystem {
		return history
	}
	out := make([]llm.Message, 0, len(history)+1)
	out = append(out, llm.SystemMessage(system))
	return append(out, history...)
}

// promptText extracts the text of a system message built by WithSystemPrompt.
func promptText(m llm.Message) string {
	var b strings.Builder
	for _, blk := range m.Content {
		if tb, ok := blk.(llm.TextBlock); ok {
			b.WriteString(tb.Text)
		}
	}
	return b.String()
}

// toolUseBlocks extracts the tool calls of an assistant message in order.
func toolUseBlocks(m llm.Message) []llm.ToolUseBlock {
	var out []llm.ToolUseBlock
	for _, blk := range m.Content {
		if tb, ok := blk.(llm.ToolUseBlock); ok {
			out = append(out, tb)
		}
	}
	return out
}
