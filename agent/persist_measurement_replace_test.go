package agent

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

func TestMeasurementReplacementClearsSameFold(t *testing.T) {
	for _, reason := range []string{"proactive_compaction", "overflow_compaction", "legacy_anchor"} {
		t.Run(reason, func(t *testing.T) {
			log := sequenceRecoveryLog([]store.Record{
				mustRecord(t, store.KindRunStarted, "r-start", runStartedPayload{RunID: "r", Input: strings.Repeat("q", 2000)}),
				measurementRecord(t, 0, llm.AssistantMessage("measured"), &llm.Usage{InputTokens: 2000}, 2),
			})
			before := measurementView(t, log)
			if measurementEstimate(before) != 2000 {
				t.Fatal("fixture has no measured prefix")
			}
			short := []llm.Message{llm.UserMessage("summary")}
			payload := historyReplacedPayload{
				RunID: "r", Round: 1, OperationID: "replace", Reason: reason, History: short,
			}
			if reason == "overflow_compaction" {
				pending := recoveryPayloadFixture()
				pending.Round, pending.BeforeRunes = 1, estimateRunes(before.history)
				pending.RejectedOutboundRunes = pending.BeforeRunes + 100
				log = append(log, recoveryRecord(t, store.KindContextRecovery, pending))
				pending.Phase, pending.Step, pending.Attempt, pending.BudgetRunes = "preparing", 1, 1, 1200
				log = append(log, recoveryRecord(t, store.KindContextRecovery, pending))
				payload.EpisodeID, payload.Attempt, payload.BudgetRunes = pending.EpisodeID, 1, 1200
				payload.BeforeRunes, payload.AfterRunes = pending.BeforeRunes, estimateRunes(short)
			}
			log = sequenceRecoveryLog(append(log, recoveryRecord(t, store.KindHistoryReplaced, payload)))
			v := measurementView(t, log)
			if v.anchor != nil || measurementEstimate(v) != estimateRunes(short) {
				t.Fatalf("replacement retained measurement: %+v", v.anchor)
			}
			cp := measurementCheckpoint(t, v)
			for _, checkpoint := range []bool{false, true} {
				records := append([]store.Record(nil), log...)
				if checkpoint {
					records = append(records, cp)
				}
				a := New(NewMockProvider(), tool.NewRegistry(), WithStore(&scriptStore{records: records}))
				got, err := a.replayThread(t.Context(), "thread")
				if err != nil || got.anchor != nil || measurementEstimate(got) != estimateRunes(short) {
					t.Fatalf("checkpoint=%v replacement estimate: %+v, %v", checkpoint, got, err)
				}
			}
		})
	}
}

func TestMeasurementLiveReplacementAndCheckpointParity(t *testing.T) {
	for _, backend := range []string{"memory", "jsonl"} {
		t.Run(backend, func(t *testing.T) {
			st, reopen := measurementBackend(t, backend)
			var buf bytes.Buffer
			a := measurementTestAgent(st, measurementRegistry(0), [][]llm.Chunk{
				contextChunks("measured", &llm.Usage{InputTokens: 2000}), textChunks("done"),
			}, &buf, WithFinishRound(contextContinue))
			p, history, err := a.openSession(t.Context(), st, "thread", "q", false, false)
			if err != nil {
				t.Fatal(err)
			}
			short := []llm.Message{llm.UserMessage("summary")}
			var liveEstimate int
			a.finishRound = func(ctx context.Context, _ FinishRoundContext) (FinishRoundDecision, error) {
				current := p.measurement
				if current != nil {
					t.Fatal("measurement was not confirmed before the hook")
				}
				original := []llm.Message{llm.UserMessage("q"), llm.AssistantMessage("measured")}
				adopted, err := p.replaceHistory(ctx, original, historyReplacedPayload{
					RunID: p.runID, Round: 1, OperationID: "replace", Reason: "proactive_compaction", History: short,
				})
				if err != nil {
					return FinishRoundDecision{}, err
				}
				liveEstimate = int(p.context.estimate(adopted))
				if err := p.checkpoint(ctx, adopted); err != nil {
					return FinishRoundDecision{}, err
				}
				return FinishRoundDecision{}, errMeasurementCrash
			}
			seq, err := a.runStreamInternal(t.Context(), history, p)
			if err != nil {
				p.release()
				t.Fatal(err)
			}
			_, runErr := a.foldRunStream(seq)
			p.release()
			if !errors.Is(runErr, errMeasurementCrash) {
				t.Fatal(runErr)
			}
			a = New(NewMockProvider(), tool.NewRegistry(), WithStore(reopen()))
			v, err := a.replayThread(t.Context(), "thread")
			if err != nil || measurementEstimate(v) != liveEstimate || liveEstimate != estimateRunes(short) {
				t.Fatalf("replacement live/replay estimate = %d/%d: %v", liveEstimate, measurementEstimate(v), err)
			}
		})
	}
}
