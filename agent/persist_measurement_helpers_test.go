package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

func measurementBackend(t *testing.T, backend string) (store.Store, func() store.Store) {
	t.Helper()
	if backend == "memory" {
		st := store.NewMemory()
		return st, func() store.Store { return st }
	}
	dir := t.TempDir()
	st, err := store.NewJSONL(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st, func() store.Store {
		t.Helper()
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
		st, err = store.NewJSONL(dir)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
}

func measurementToolChunks(usage *llm.Usage) []llm.Chunk {
	chunks := parallelToolRound([]*llm.ToolUseBlock{
		{Type: "tool_use", ID: "one", Name: "echo", Input: json.RawMessage(`{}`)},
		{Type: "tool_use", ID: "two", Name: "echo", Input: json.RawMessage(`{}`)},
	})
	chunks[len(chunks)-1] = &llm.DoneChunk{Usage: usage}
	return chunks
}

func measurementRegistry(realPrefix int) *tool.Registry {
	reg := tool.NewRegistry()
	calls := 0
	reg.MustRegister(mutatorTool("echo", func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		calls++
		if calls <= realPrefix {
			return tool.NewTextResult("recorded result"), nil
		}
		unknown := unknownResults(toolCallMessage("unused"))
		block := unknown[0].Content[0].(*llm.ToolResultBlock)
		return tool.NewErrorResult("%s", block.Content), nil
	}))
	return reg
}

func measurementTestAgent(st store.Store, reg *tool.Registry, steps [][]llm.Chunk, buf *bytes.Buffer, opts ...Option) *Agent {
	options := []Option{
		WithStore(st), WithLLMOptions(llm.WithModel("x")),
		WithLogger(contextTestLogger(buf)), WithCompactor(nil),
	}
	options = append(options, opts...)
	return New(NewMockStreamingProvider(steps), reg, options...)
}

var errMeasurementCrash = errors.New("measurement interruption")

type measurementFaultStore struct {
	store.Store
	kind  string
	after bool
	fired bool
}

func (s *measurementFaultStore) Append(ctx context.Context, thread string, expected int64, records ...store.Record) (int64, error) {
	if !s.fired && records[0].Kind == s.kind {
		s.fired = true
		if s.after {
			if _, err := s.Store.Append(ctx, thread, expected, records...); err != nil {
				return 0, err
			}
		}
		return 0, errMeasurementCrash
	}
	return s.Store.Append(ctx, thread, expected, records...)
}

func assertMeasurementBudget(t *testing.T, got, want contextBudgetRecord) {
	t.Helper()
	if got.Estimate != want.Estimate || got.Valid != want.Valid ||
		got.AnchorTokens != want.AnchorTokens || got.Tail != want.Tail {
		t.Fatalf("resumed estimate %+v, uninterrupted %+v", got, want)
	}
}

func measurementRecord(t *testing.T, round int, msg llm.Message, usage *llm.Usage, count int) store.Record {
	t.Helper()
	m := requestMeasurement(NewMockStreamingProvider(nil),
		[]llm.Option{llm.WithModel("x")}, measurementRegistry(0).List())
	p := roundDeclaredPayload{
		RunID: "r", Round: round, Message: msg,
		ContextMeasurement: &declaredMeasurement{
			measurementIdentity: measurementIdentityFor(m, count), Usage: usage, CaptureConfigSeq: -1,
		},
	}
	if len(toolUseBlocks(msg)) == 0 {
		p.Kind = terminalTextDeclaration
	}
	rec := mustRecord(t, store.KindRoundDeclared, recordID("r", "-d"+jsonNumber(round)), p)
	rec.Schema = store.SchemaV5
	return rec
}

func measurementView(t *testing.T, records []store.Record) threadView {
	t.Helper()
	v, err := replayRecords(threadView{lastRound: -1, cfgSeq: -1}, sequenceRecoveryLog(records))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func measurementEstimate(v threadView) int {
	if v.open != nil {
		v.closeUnknown()
	}
	history := seededHistory(v.system, v.history)
	c := &contextRuntime{}
	c.restoreAnchor(v.anchor, history)
	return int(c.estimate(history))
}
