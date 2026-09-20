package agent

import (
	"context"
	"encoding/json"
	"iter"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

// persistTestAgent builds an agent with an echo tool, a scripted provider,
// and the given store.
func persistTestAgent(t *testing.T, st store.Store, responses ...mockResponse) *Agent {
	t.Helper()
	reg := tool.NewRegistry()
	reg.MustRegister(&mockTool{
		info:   tool.ToolInfo{Name: "echo", Description: "echoes"},
		result: tool.NewTextResult("echoed"),
	})
	return New(NewMockProvider(responses...), reg, WithStore(st), WithLogger(discardLogger()))
}

func toolCallMessage(ids ...string) llm.Message {
	calls := make([]llm.ToolUseBlock, len(ids))
	for i, id := range ids {
		calls[i] = llm.ToolUseBlock{Type: "tool_use", ID: id, Name: "echo", Input: json.RawMessage(`{}`)}
	}
	return llm.AssistantToolCallMessage(calls...)
}

// logRecords returns the thread's full record log with checkpoint records
// removed (they are accelerators, not history).
func logRecords(t *testing.T, st store.Store, thread string) []store.Record {
	t.Helper()
	all, err := st.History(context.Background(), thread, 0)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	out := all[:0]
	for _, r := range all {
		if r.Kind != store.KindCheckpoint {
			out = append(out, r)
		}
	}
	return out
}

func logKinds(t *testing.T, st store.Store, thread string) []string {
	t.Helper()
	records := logRecords(t, st, thread)
	kinds := make([]string, len(records))
	for i, r := range records {
		kinds[i] = r.Kind
	}
	return kinds
}

func equalKinds(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("record kinds = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("record kinds = %v, want %v", got, want)
		}
	}
}

// writeRaw records a lifecycle record directly in the store, bypassing the
// agent, to stage crash states deterministically.
func writeRaw(t *testing.T, st store.Store, thread string, expected int64, rec store.Record) int64 {
	t.Helper()
	head, err := st.Append(context.Background(), thread, expected, rec)
	if err != nil {
		t.Fatalf("stage record %s: %v", rec.Kind, err)
	}
	return head
}

func stagedRunStarted(t *testing.T, st store.Store, thread, runID, input string) int64 {
	t.Helper()
	rec, err := encodeRecord(store.KindRunStarted, recordID(runID, "-start"),
		runStartedPayload{RunID: runID, Input: input})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return writeRaw(t, st, thread, 0, rec)
}

func stagedDeclaration(t *testing.T, st store.Store, thread, runID string, expected int64, round int, msg llm.Message) int64 {
	t.Helper()
	rec, err := encodeRecord(store.KindRoundDeclared, recordID(runID, "-d"+jsonNumber(round)),
		roundDeclaredPayload{RunID: runID, Round: round, Message: msg})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return writeRaw(t, st, thread, expected, rec)
}

func jsonNumber(n int) string {
	return string([]byte{byte('0' + n)})
}

func resultBlocks(msg llm.Message) []llm.ToolResultBlock {
	return toolResultBlocks(msg)
}

func messageString(m llm.Message) string {
	s := ""
	for _, blk := range m.Content {
		if tb, ok := blk.(llm.TextBlock); ok {
			s += tb.Text
		}
	}
	return s
}

// envelopeOf decodes an agent_event record's envelope.
func envelopeOf(t *testing.T, r store.Record) eventEnvelope {
	t.Helper()
	var env eventEnvelope
	if err := strictDecode(r.Payload, &env); err != nil {
		t.Fatalf("envelope: %v", err)
	}
	return env
}

// gateProvider signals entered on its first ChatStream call and then fails
// streaming like a non-streaming provider; Chat blocks until release is
// closed. It holds a run at a known point: after ownership, before the
// answer.
type gateProvider struct {
	entered  chan struct{}
	release  chan struct{}
	reply    llm.Message
	signaled bool
}

func (g *gateProvider) Name() string { return "gate" }

func (g *gateProvider) Chat(_ context.Context, _ []llm.Message, _ []tool.ToolInfo, _ ...llm.Option) (*llm.Message, *llm.Usage, error) {
	<-g.release
	return &g.reply, nil, nil
}

func (g *gateProvider) ChatStream(_ context.Context, _ []llm.Message, _ []tool.ToolInfo, _ ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	if !g.signaled {
		g.signaled = true
		g.entered <- struct{}{}
	}
	return nil, llm.ErrStreamingNotSupported
}
