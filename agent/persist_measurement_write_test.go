package agent

import (
	"bytes"
	"context"
	"errors"
	"math"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
)

type measurementRetryStore struct {
	store.Store
	kind     string
	attempts []store.Record
}

func (s *measurementRetryStore) Append(ctx context.Context, thread string, expected int64, records ...store.Record) (int64, error) {
	if records[0].Kind == s.kind {
		copy := records[0]
		copy.Payload = bytes.Clone(copy.Payload)
		s.attempts = append(s.attempts, copy)
		if len(s.attempts) == 1 {
			r, err := encodeRecord(store.KindInputQueued, "queued-control", inputQueuedPayload{Input: "control"})
			if err != nil {
				return 0, err
			}
			r.Schema = store.SchemaV2
			if _, err := s.Store.Append(ctx, thread, expected, r); err != nil {
				return 0, err
			}
		}
	}
	return s.Store.Append(ctx, thread, expected, records...)
}

func TestMeasurementRebaseRetriesImmutablePayload(t *testing.T) {
	for _, backend := range []string{"memory", "jsonl"} {
		for _, kind := range []string{store.KindRoundDeclared, store.KindAgentEvent} {
			t.Run(backend+"/"+kind, func(t *testing.T) {
				base, reopen := measurementBackend(t, backend)
				st := &measurementRetryStore{Store: base, kind: kind}
				var buf bytes.Buffer
				a := measurementTestAgent(st, measurementRegistry(0), [][]llm.Chunk{
					measurementToolChunks(&llm.Usage{InputTokens: 2000}),
					contextChunks("done", &llm.Usage{InputTokens: 3000}),
				}, &buf)
				if _, err := a.Run(t.Context(), "q", WithThread("thread")); err != nil {
					t.Fatal(err)
				}
				if len(st.attempts) != 2 {
					t.Fatalf("rebase attempts = %d, want 2", len(st.attempts))
				}
				first, second := st.attempts[0], st.attempts[1]
				if first.ID != second.ID || first.Schema != second.Schema ||
					first.Kind != second.Kind || !bytes.Equal(first.Payload, second.Payload) {
					t.Fatal("response retry payload changed after absorption")
				}
				// A repeated ambiguous caller write uses Store's existing
				// complete-content idempotency, not a new measurement write.
				reopened := reopen()
				log, err := reopened.History(t.Context(), "thread", 0)
				if err != nil {
					t.Fatal(err)
				}
				matches := 0
				for _, r := range log {
					if r.ID != first.ID {
						continue
					}
					matches++
					if _, err := reopened.Append(t.Context(), "thread", r.Seq, first); err != nil {
						t.Fatalf("immutable ambiguous retry: %v", err)
					}
				}
				if matches != 1 {
					t.Fatalf("response records = %d, want one", matches)
				}
				a = New(nil, nil, WithStore(reopened))
				v, err := a.replayThread(t.Context(), "thread")
				if err != nil || measurementEstimate(v) != 3000 {
					t.Fatalf("retry/reopen measurement: %d, %v", measurementEstimate(v), err)
				}
			})
		}
	}
}

type measurementGateStore struct {
	store.Store
	session *persistence
	check   func(*persistence)
}

func (s *measurementGateStore) Append(ctx context.Context, thread string, expected int64, records ...store.Record) (int64, error) {
	if records[0].Kind == store.KindAgentEvent {
		s.check(s.session)
		return 0, errMeasurementCrash
	}
	return s.Store.Append(ctx, thread, expected, records...)
}

func TestMeasurementActivationRequiresDurableConfirmation(t *testing.T) {
	base := store.NewMemory()
	st := &measurementGateStore{Store: base}
	checked := false
	st.check = func(p *persistence) {
		checked = true
		if p.context.anchor == nil || p.context.anchor.inputTokens != 2000 {
			t.Fatalf("unconfirmed response replaced the live boundary: %+v", p.context.anchor)
		}
	}
	var buf bytes.Buffer
	a := measurementTestAgent(st, measurementRegistry(0), [][]llm.Chunk{
		contextChunks("first", &llm.Usage{InputTokens: 2000}),
		contextChunks("unconfirmed", &llm.Usage{InputTokens: 3000}),
	}, &buf, WithMaxIter(2), WithFinishRound(contextContinue))
	p, history, err := a.openSession(t.Context(), st, "thread", "q", false, false)
	if err != nil {
		t.Fatal(err)
	}
	st.session = p
	seq, err := a.runStreamInternal(t.Context(), history, p)
	if err != nil {
		p.release()
		t.Fatal(err)
	}
	_, runErr := a.foldRunStream(seq)
	p.release()
	if !checked || !errors.Is(runErr, errMeasurementCrash) {
		t.Fatalf("durable gate not exercised: %v/%v", checked, runErr)
	}
	v, err := a.replayThread(t.Context(), "thread")
	if err != nil || measurementEstimate(v) != 2000 || historyContainsText(v.history, "unconfirmed") {
		t.Fatalf("failed response contaminated replay: %+v, %v", v, err)
	}
}

func TestMeasurementFingerprintFailureStillWritesResponse(t *testing.T) {
	st := store.NewMemory()
	var buf bytes.Buffer
	a := measurementTestAgent(st, measurementRegistry(0),
		[][]llm.Chunk{contextChunks("answer", &llm.Usage{InputTokens: 2000})}, &buf,
		WithLLMOptions(llm.WithModel("x"), llm.WithTemperature(math.NaN())))
	if _, err := a.Run(t.Context(), "q", WithThread("thread")); err != nil {
		t.Fatalf("accounting encoding failure aborted request: %v", err)
	}
	log := logRecords(t, st, "thread")
	p, err := decodeAgentEvent(log[len(log)-1])
	if err != nil || p.ContextMeasurement == nil || p.ContextMeasurement.RequestFingerprint != nil {
		t.Fatalf("fingerprint failure descriptor = %+v, %v", p.ContextMeasurement, err)
	}
	v, err := a.replayThread(t.Context(), "thread")
	if err != nil || v.anchor != nil || measurementEstimate(v) != estimateRunes(v.history) {
		t.Fatalf("null fingerprint established boundary: %+v, %v", v.anchor, err)
	}
}
