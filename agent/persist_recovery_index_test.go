package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
)

func episodeIndexLog(t *testing.T, n int) []store.Record {
	t.Helper()
	config := mustRecord(t, store.KindConfigChanged, "config", configChangedPayload{Model: strptr("A")})
	config.Schema = store.SchemaV2
	log := []store.Record{config}
	history := []llm.Message{llm.UserMessage("canonical")}
	for i := range n {
		run := fmt.Sprintf("run-%d", i)
		pending := recoveryPayloadFixture()
		pending.RunID, pending.EpisodeID, pending.Model, pending.ConfigSeq = run, run+"-episode", "A", 0
		pending.BeforeRunes, pending.RejectedOutboundRunes = estimateRunes(history), estimateRunes(history)
		log = append(log,
			mustRecord(t, store.KindRunStarted, run+"-start", runStartedPayload{RunID: run, Input: "input"}),
			recoveryRecord(t, store.KindHistoryReplaced, historyReplacedPayload{
				RunID: run, OperationID: run + "-replace", Reason: "proactive_compaction", History: history,
			}),
			recoveryRecord(t, store.KindContextRecovery, pending),
			store.Record{Kind: store.KindRunCancelled, Schema: store.SchemaV2, ID: run + "-cancel",
				Payload: mustJSON(t, runCancelledPayload{RunID: run, Results: []llm.Message{}})},
		)
	}
	return sequenceRecoveryLog(log)
}

func TestRecoveryEpisodeReuseRejectedAcrossCheckpoint(t *testing.T) {
	for _, checkpoint := range []bool{false, true} {
		t.Run(fmt.Sprintf("checkpoint=%t", checkpoint), func(t *testing.T) {
			log := episodeIndexLog(t, 3)
			cancel := log[len(log)-1]
			log = log[:len(log)-1]
			v, err := replayRecords(threadView{lastRound: -1, cfgSeq: -1}, log)
			if err != nil {
				t.Fatal(err)
			}
			if checkpoint {
				cp := recoveryCheckpointRecord(t, v)
				cp.ID = "checkpoint"
				log = append(log, cp)
			}
			start := mustRecord(t, store.KindRunStarted, "next-start", runStartedPayload{RunID: "next", Input: "next"})
			pending := recoveryPayloadFixture()
			pending.RunID, pending.EpisodeID, pending.Model, pending.ConfigSeq = "next", "run-0-episode", "A", 0
			pending.BeforeRunes = estimateRunes(append(deepCopyMessages(v.history), llm.UserMessage("next")))
			pending.RejectedOutboundRunes = pending.BeforeRunes
			log = sequenceRecoveryLog(append(log, cancel, start, recoveryRecord(t, store.KindContextRecovery, pending)))
			a := New(nil, nil, WithStore(&scriptStore{records: log}))
			if _, err := a.replayThread(t.Context(), "t"); !errors.Is(err, ErrIncompatibleLog) {
				t.Fatalf("episode index lost at checkpoint boundary: %v", err)
			}
		})
	}
}

func TestRecoveryEpisodeReplayAllocationsScaleLinearly(t *testing.T) {
	var counts [2]float64
	for i, n := range []int{32, 64} {
		log := episodeIndexLog(t, n)
		var replayErr error
		counts[i] = testing.AllocsPerRun(1, func() {
			_, replayErr = replayRecords(threadView{lastRound: -1, cfgSeq: -1}, log)
		})
		if replayErr != nil {
			t.Fatal(replayErr)
		}
	}
	t.Logf("episode replay allocations: 32=%g, 64=%g", counts[0], counts[1])
	if counts[1] > 3*counts[0] {
		t.Fatalf("doubling episodes redecodes prior transitions: %v", counts)
	}
}

type recoveryNoHistoryStore struct {
	store.Store
	reject bool
}

func (s *recoveryNoHistoryStore) History(ctx context.Context, thread string, from int64) ([]store.Record, error) {
	if s.reject {
		return nil, errors.New("unexpected recovery log read")
	}
	return s.Store.History(ctx, thread, from)
}

func TestRecoverySessionIndexesAvoidEpisodeLogReads(t *testing.T) {
	for _, reuse := range []bool{false, true} {
		t.Run(fmt.Sprintf("reuse=%t", reuse), func(t *testing.T) {
			st := &recoveryNoHistoryStore{Store: store.NewMemory()}
			if _, err := st.Append(t.Context(), "t", 0, episodeIndexLog(t, 3)...); err != nil {
				t.Fatal(err)
			}
			a := New(nil, nil, WithStore(st), WithLogger(discardLogger()))
			sess, history, err := a.openSession(t.Context(), st, "t", "next", false, false)
			if err != nil {
				t.Fatal(err)
			}
			defer sess.release()
			st.reject = true
			p := recoveryPayloadFixture()
			p.RunID, p.Model, p.ConfigSeq = sess.runID, "A", 0
			if reuse {
				p.EpisodeID = "run-0-episode"
			}
			p.BeforeRunes, p.RejectedOutboundRunes = estimateRunes(history), estimateRunes(history)
			r := overflowRecovery{a: a, sess: sess, round: 0, cfgSeq: 0}
			err = r.save(t.Context(), history, p, nil)
			if reuse {
				if !errors.Is(err, ErrIncompatibleLog) {
					t.Fatalf("session forgot prior episode: %v", err)
				}
			} else if err != nil || r.state == nil || r.state.ConfigSeq != 0 {
				t.Fatalf("session reread log or lost config index: %+v, %v", r.state, err)
			}
		})
	}
}

func replacementIndexLog(t *testing.T, n int) []store.Record {
	t.Helper()
	log := []store.Record{mustRecord(t, store.KindRunStarted, "r-start", runStartedPayload{RunID: "r", Input: "original"})}
	for i := range n {
		id := fmt.Sprintf("operation-%d", i)
		history := []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{
			llm.TextBlock{Type: "text", Text: id},
			llm.ImageBlock{Type: "image", MIMEType: "image/png", Data: bytes.Repeat([]byte{1}, 128)},
		}}}
		r := recoveryRecord(t, store.KindHistoryReplaced, historyReplacedPayload{
			RunID: "r", OperationID: id, Reason: "proactive_compaction", History: history,
		})
		r.ID, r.Seq = id, int64(len(log))
		log = append(log, r)
	}
	return log
}

func TestRecoveryReplacementRetryIdentityAcrossCheckpoint(t *testing.T) {
	for _, checkpoint := range []bool{false, true} {
		for _, conflict := range []bool{false, true} {
			t.Run(fmt.Sprintf("checkpoint=%t/conflict=%t", checkpoint, conflict), func(t *testing.T) {
				log := replacementIndexLog(t, 2)
				want, err := replayRecords(threadView{lastRound: -1, cfgSeq: -1}, log)
				if err != nil {
					t.Fatal(err)
				}
				retry := log[1]
				if checkpoint {
					cp := recoveryCheckpointRecord(t, want)
					cp.ID, cp.Seq = "checkpoint", int64(len(log))
					log = append(log, cp)
				}
				if conflict {
					retry = mutateRecoveryJSON(t, retry, "history", mustJSON(t, []llm.Message{llm.UserMessage("conflicting")}))
				}
				retry.Seq = int64(len(log))
				a := New(nil, nil, WithStore(&scriptStore{records: append(log, retry)}))
				got, err := a.replayThread(t.Context(), "t")
				if conflict {
					if !errors.Is(err, ErrIncompatibleLog) {
						t.Fatalf("operation reuse accepted: %v", err)
					}
				} else if err != nil || !reflect.DeepEqual(got.history, want.history) {
					t.Fatalf("identical old retry rolled history back: %+v, %v", got.history, err)
				}
			})
		}
	}
}

func TestRecoveryReplacementReplayAllocationsScaleLinearly(t *testing.T) {
	var counts [2]float64
	for i, n := range []int{32, 64} {
		log := replacementIndexLog(t, n)
		var replayErr error
		counts[i] = testing.AllocsPerRun(1, func() {
			_, replayErr = replayRecords(threadView{lastRound: -1, cfgSeq: -1}, log)
		})
		if replayErr != nil {
			t.Fatal(replayErr)
		}
	}
	t.Logf("replacement replay allocations: 32=%g, 64=%g", counts[0], counts[1])
	if counts[1] > 3*counts[0] {
		t.Fatalf("doubling replacements redecodes prior histories: %v", counts)
	}
}
