package agent

import (
	"bytes"
	"context"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
)

func TestMeasurementCancellationKeepsResponseBoundary(t *testing.T) {
	for _, recorded := range []int{0, 1} {
		t.Run(jsonNumber(recorded), func(t *testing.T) {
			st := store.NewMemory()
			var buf bytes.Buffer
			a := measurementTestAgent(st, measurementRegistry(recorded),
				[][]llm.Chunk{measurementToolChunks(&llm.Usage{InputTokens: 2000})}, &buf)
			seq, err := a.RunStream(t.Context(), "q", WithThread("thread"))
			if err != nil {
				t.Fatal(err)
			}
			for event, err := range seq {
				if err != nil {
					t.Fatal(err)
				}
				if _, ok := event.(ToolCallEvent); ok && recorded == 0 {
					break
				}
				if _, ok := event.(ToolResultEvent); ok && recorded == 1 {
					break
				}
			}
			v, err := a.replayThread(t.Context(), "thread")
			if err != nil {
				t.Fatal(err)
			}
			want := measurementEstimate(v)
			if err := a.SettleThread(t.Context(), SettlementToken{
				ThreadID: "thread", RunID: v.runID, ExpectedHead: v.head,
			}); err != nil {
				t.Fatal(err)
			}
			cancelled, err := a.replayThread(t.Context(), "thread")
			if err != nil || !cancelled.cancelled || measurementEstimate(cancelled) != want {
				t.Fatalf("cancellation moved measured boundary: %+v, %v", cancelled, err)
			}
			var next bytes.Buffer
			a = measurementTestAgent(st, measurementRegistry(recorded), [][]llm.Chunk{textChunks("done")}, &next)
			if _, err := a.Run(t.Context(), "next", WithThread("thread")); err != nil {
				t.Fatal(err)
			}
			decisions := contextBudgetRecords(t, &next)
			want += appendRunesDelta(cancelled.history, []llm.Message{llm.UserMessage("next")})
			if len(decisions) != 1 || !decisions[0].Valid || decisions[0].Estimate != want {
				t.Fatalf("new run after cancellation: %+v, want %d", decisions, want)
			}
		})
	}
}

func TestMeasurementDoneWriterDistinguishesNilAndZero(t *testing.T) {
	for _, tc := range []struct {
		name  string
		usage *llm.Usage
	}{
		{"nil", nil}, {"zero", &llm.Usage{}}, {"eligible", &llm.Usage{InputTokens: 2000}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := store.NewMemory()
			if err := SetThreadConfig(t.Context(), st, "thread", ConfigChange{Model: strptr("x")}); err != nil {
				t.Fatal(err)
			}
			var buf bytes.Buffer
			a := measurementTestAgent(st, measurementRegistry(0), [][]llm.Chunk{contextChunks("done", tc.usage)}, &buf)
			if _, err := a.Run(t.Context(), "q", WithThread("thread")); err != nil {
				t.Fatal(err)
			}
			log := logRecords(t, st, "thread")
			p, err := decodeAgentEvent(log[len(log)-1])
			if err != nil {
				t.Fatal(err)
			}
			if p.ContextMeasurement == nil || p.ContextMeasurement.UsageReported != (tc.usage != nil) ||
				p.ContextMeasurement.CaptureConfigSeq != 0 {
				t.Fatalf("Done lost usage presence or frozen config sequence: %+v", p)
			}
		})
	}
}

type measurementDoneControlStore struct {
	store.Store
	fired bool
}

func (s *measurementDoneControlStore) Append(ctx context.Context, thread string, expected int64, records ...store.Record) (int64, error) {
	if records[0].Kind == store.KindAgentEvent && !s.fired {
		s.fired = true
		if err := SetThreadConfig(ctx, s.Store, thread, ConfigChange{Thinking: strptr("enabled")}); err != nil {
			return 0, err
		}
	}
	return s.Store.Append(ctx, thread, expected, records...)
}

func TestMeasurementDoneAbsorptionDoesNotRestoreStaleUsage(t *testing.T) {
	st := &measurementDoneControlStore{Store: store.NewMemory()}
	var buf bytes.Buffer
	a := measurementTestAgent(st, measurementRegistry(0),
		[][]llm.Chunk{contextChunks("done", &llm.Usage{InputTokens: 2000})}, &buf,
		WithLLMOptions(llm.WithModel("x"), llm.WithThinking("enabled")))
	result, err := a.Run(t.Context(), "q", WithThread("thread"))
	if err != nil {
		t.Fatal(err)
	}
	var next bytes.Buffer
	a = measurementTestAgent(st, measurementRegistry(0), [][]llm.Chunk{textChunks("next")}, &next,
		WithLLMOptions(llm.WithModel("x"), llm.WithThinking("enabled")))
	if _, err := a.Run(t.Context(), "new input", WithThread("thread")); err != nil {
		t.Fatal(err)
	}
	decisions := contextBudgetRecords(t, &next)
	want := estimateRunes(append(result.History, llm.UserMessage("new input")))
	if len(decisions) != 1 || decisions[0].Valid || decisions[0].Estimate != want {
		t.Fatalf("Done absorption restored stale boundary: %+v, want %d", decisions, want)
	}
}
