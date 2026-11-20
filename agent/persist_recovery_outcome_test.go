package agent

import (
	"errors"
	"reflect"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
)

func TestRecoveryCompactorFailureMayBlockBeforeExhaustion(t *testing.T) {
	for _, reason := range []string{"compactor_error", "no_progress", "invalid_compaction"} {
		for _, attempt := range []int{0, 1} {
			t.Run(reason+"/attempt "+jsonNumber(attempt), func(t *testing.T) {
				log := recoveryLogFixture(t)[:2+attempt]
				p, err := decodeContextRecovery(log[len(log)-1])
				if err != nil {
					t.Fatal(err)
				}
				p.Phase, p.Reason, p.Step = "blocked", reason, p.Step+1
				log = append(log, recoveryRecord(t, store.KindContextRecovery, p))
				v, err := replayRecords(threadView{lastRound: -1, cfgSeq: -1}, sequenceRecoveryLog(log))
				if err != nil || v.recovery == nil || v.recovery.Phase != "blocked" ||
					v.recovery.Attempt != attempt || v.recovery.Reason != reason {
					t.Fatalf("early compactor failure must remain blocked: %+v, %v", v.recovery, err)
				}
			})
		}
	}
}

func TestRecoveryProviderFailureNote(t *testing.T) {
	log := recoveryLogFixture(t)
	p, err := decodeContextRecovery(log[4])
	if err != nil {
		t.Fatal(err)
	}
	p.Step, p.Reason = 3, "provider_error"
	log = append(log, recoveryRecord(t, store.KindContextRecovery, p))
	failed, err := replayRecords(threadView{lastRound: -1, cfgSeq: -1}, sequenceRecoveryLog(log))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		record store.Record
	}{
		{"normal done", recoveryDone(t, "r-done", false)},
		{"truncated done", recoveryDone(t, "r-done", true)},
		{"terminal text", mustRecord(t, store.KindRoundDeclared, "r-d0", roundDeclaredPayload{
			RunID: "r", Round: 0, Message: llm.AssistantMessage("not success"), Kind: terminalTextDeclaration,
		})},
		{"tool declaration", mustRecord(t, store.KindRoundDeclared, "r-d0", roundDeclaredPayload{
			RunID: "r", Round: 0, Message: toolCallMessage("not success"),
		})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.record.Seq = int64(len(log))
			v, err := replayRecords(failed, []store.Record{tc.record})
			if !errors.Is(err, ErrIncompatibleLog) || v.recovery == nil ||
				v.recovery.Reason != "provider_error" || !reflect.DeepEqual(v.history, failed.history) {
				t.Fatalf("success after failure note = %+v, %v", v, err)
			}
		})
	}
	for _, tc := range []struct {
		name, phase, reason string
		attempt, budget     int
		reject              bool
	}{
		{"next slot", "preparing", "resume_after_provider_error", 2, 400, false},
		{"invented interruption", "preparing", "interrupted_attempt", 2, 400, true},
		{"same slot", "preparing", "resume_after_provider_error", 1, 400, true},
		{"fake new overflow", "pending", "", 1, 1200, true},
		{"repeat failure note", "dispatched", "provider_error", 1, 1200, true},
		{"bare ready", "ready", "", 1, 1200, true},
		{"premature permanent block", "blocked", "no_progress", 1, 1200, true},
		{"resume budget exhausted", "blocked", "budget_exhausted", 1, 1200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next := p
			next.Step, next.Phase, next.Reason = 4, tc.phase, tc.reason
			next.Attempt, next.BudgetRunes = tc.attempt, tc.budget
			if next.Phase == "preparing" {
				next.BeforeRunes = estimateRunes(failed.history)
			}
			r := recoveryRecord(t, store.KindContextRecovery, next)
			r.Seq = int64(len(log))
			v, err := replayRecords(failed, []store.Record{r})
			if tc.reject {
				if !errors.Is(err, ErrIncompatibleLog) || v.recovery.Reason != "provider_error" {
					t.Fatalf("invalid failure-note transition: %+v, %v", v.recovery, err)
				}
			} else if err != nil || v.recovery.Phase != tc.phase || v.recovery.Attempt != tc.attempt {
				t.Fatalf("allowed failure-note transition: %+v, %v", v.recovery, err)
			}
		})
	}
	t.Run("cancellation", func(t *testing.T) {
		r, err := failed.cancelRecord()
		if err != nil {
			t.Fatal(err)
		}
		r.Seq = int64(len(log))
		v, err := replayRecords(failed, []store.Record{r})
		if err != nil || v.recovery != nil || !v.cancelled {
			t.Fatalf("cancel failed recovery: %+v, %v", v, err)
		}
	})
}

func TestRecoverySecondSlotAndExhaustion(t *testing.T) {
	for _, source := range []string{"overflow", "provider_error", "interrupted_attempt"} {
		t.Run(source, func(t *testing.T) {
			log := recoveryLogFixture(t)
			p, err := decodeContextRecovery(log[4])
			if err != nil {
				t.Fatal(err)
			}
			v, err := replayRecords(threadView{lastRound: -1, cfgSeq: -1}, log)
			if err != nil {
				t.Fatal(err)
			}
			if source != "interrupted_attempt" {
				p.Step++
				if source == "overflow" {
					p.Phase, p.Reason = "pending", ""
					p.BeforeRunes = estimateRunes(v.history)
					p.RejectedOutboundRunes = p.BeforeRunes + 100
				} else {
					p.Reason = "provider_error"
				}
				log = append(log, recoveryRecord(t, store.KindContextRecovery, p))
			}
			p.Step++
			p.Phase, p.Attempt, p.BudgetRunes = "preparing", 2, 400
			p.BeforeRunes = estimateRunes(v.history)
			switch source {
			case "provider_error":
				p.Reason = "resume_after_provider_error"
			case "interrupted_attempt":
				p.Reason = source
			}
			log = append(log, recoveryRecord(t, store.KindContextRecovery, p))
			small := []llm.Message{llm.UserMessage("smallest history")}
			log = append(log, recoveryRecord(t, store.KindHistoryReplaced, historyReplacedPayload{
				RunID: "r", OperationID: "second-op", Reason: "overflow_compaction", History: small,
				EpisodeID: p.EpisodeID, Attempt: 2, BudgetRunes: 400,
				BeforeRunes: p.BeforeRunes, AfterRunes: estimateRunes(small),
			}))
			p.Step++
			p.Phase, p.Reason = "dispatched", ""
			log = append(log, recoveryRecord(t, store.KindContextRecovery, p))
			p.Step++
			p.Reason = "provider_error"
			log = append(log, recoveryRecord(t, store.KindContextRecovery, p))
			p.Step++
			p.Phase, p.Reason = "blocked", "attempts_exhausted"
			log = append(log, recoveryRecord(t, store.KindContextRecovery, p))
			v, err = replayRecords(threadView{lastRound: -1, cfgSeq: -1}, sequenceRecoveryLog(log))
			if err != nil || v.recovery == nil || v.recovery.Phase != "blocked" ||
				v.recovery.Attempt != 2 || messageString(v.history[0]) != "smallest history" {
				t.Fatalf("bounded second slot = %+v, %v", v.recovery, err)
			}
		})
	}
}

func TestRecoveryOldRunDoneCannotEndSuccessor(t *testing.T) {
	oldStart := mustRecord(t, store.KindRunStarted, "a-start", runStartedPayload{RunID: "a", Input: "old"})
	oldCancel := store.Record{Kind: store.KindRunCancelled, Schema: store.SchemaV2, ID: "a-cancel",
		Payload: mustJSON(t, runCancelledPayload{RunID: "a", Results: []llm.Message{}})}
	log := append([]store.Record{oldStart, oldCancel}, recoveryLogFixture(t)...)
	// Run B's history also contains run A's user input; make its measurements
	// match that real prefix so only the trailing old Done is invalid.
	oldHistory := []llm.Message{llm.UserMessage("old")}
	for _, i := range []int{3, 4, 6} {
		p, err := decodeContextRecovery(log[i])
		if err != nil {
			t.Fatal(err)
		}
		start, err := decodeRunStarted(log[2])
		if err != nil {
			t.Fatal(err)
		}
		p.BeforeRunes = estimateRunes(append(oldHistory, llm.UserMessage(start.Input)))
		log[i] = recoveryRecord(t, store.KindContextRecovery, p)
	}
	replacement, err := decodeHistoryReplaced(log[5])
	if err != nil {
		t.Fatal(err)
	}
	preparing, err := decodeContextRecovery(log[4])
	if err != nil {
		t.Fatal(err)
	}
	replacement.BeforeRunes = preparing.BeforeRunes
	log[5] = recoveryRecord(t, store.KindHistoryReplaced, replacement)
	before, err := replayRecords(threadView{lastRound: -1, cfgSeq: -1}, sequenceRecoveryLog(log))
	if err != nil {
		t.Fatal(err)
	}
	done := recoveryDone(t, "a-done", false)
	done.Seq = int64(len(log))
	after, err := replayRecords(before, []store.Record{done})
	if !errors.Is(err, ErrIncompatibleLog) || !after.runActive || after.runID != "r" ||
		after.recovery == nil || after.recovery.Phase != "dispatched" || !reflect.DeepEqual(before.history, after.history) {
		t.Fatalf("old Done ended successor: %+v, %v", after, err)
	}
}

func TestRecoveryLegacyAnchorAndOperationIdentity(t *testing.T) {
	log := recoveryLogFixture(t)
	start, err := decodeRunStarted(log[0])
	if err != nil {
		t.Fatal(err)
	}
	anchorHistory := []llm.Message{llm.UserMessage(start.Input)}
	anchor := recoveryRecord(t, store.KindHistoryReplaced, historyReplacedPayload{
		RunID: "r", OperationID: "anchor", Reason: "legacy_anchor", History: anchorHistory,
	})
	start.Input = "raw history before the old snapshot"
	log[0] = mustRecord(t, store.KindRunStarted, "r-start", start)
	log = append(log[:1], append([]store.Record{anchor}, log[1:]...)...)
	v, err := replayRecords(threadView{lastRound: -1, cfgSeq: -1}, sequenceRecoveryLog(log))
	if err != nil || v.recovery == nil || v.recovery.Phase != "dispatched" ||
		messageString(v.history[0]) == start.Input {
		t.Fatalf("anchored recovery = %+v, %v", v, err)
	}
	t.Run("duplicate operation id", func(t *testing.T) {
		p, err := decodeHistoryReplaced(log[4])
		if err != nil {
			t.Fatal(err)
		}
		p.OperationID = "anchor"
		bad := append([]store.Record(nil), log...)
		bad[4] = recoveryRecord(t, store.KindHistoryReplaced, p)
		_, err = replayRecords(threadView{lastRound: -1, cfgSeq: -1}, sequenceRecoveryLog(bad))
		if !errors.Is(err, ErrIncompatibleLog) {
			t.Fatalf("reused operation id: %v", err)
		}
	})
}
