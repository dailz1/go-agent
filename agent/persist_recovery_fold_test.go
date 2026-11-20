package agent

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
)

func recoveryLogFixture(t *testing.T) []store.Record {
	t.Helper()
	input := strings.Repeat("x", 2000)
	p := recoveryPayloadFixture()
	p.BeforeRunes = estimateRunes([]llm.Message{llm.UserMessage(input)})
	p.RejectedOutboundRunes = p.BeforeRunes + 100
	records := []store.Record{
		mustRecord(t, store.KindRunStarted, "r-start", runStartedPayload{RunID: "r", Input: input}),
		recoveryRecord(t, store.KindContextRecovery, p),
	}
	p.Phase, p.Step, p.Attempt, p.BudgetRunes = "preparing", 1, 1, 1200
	records = append(records, recoveryRecord(t, store.KindContextRecovery, p))
	history := []llm.Message{llm.UserMessage(strings.Repeat("s", 600))}
	records = append(records, recoveryRecord(t, store.KindHistoryReplaced, historyReplacedPayload{
		RunID: "r", Round: 0, OperationID: "op", Reason: "overflow_compaction",
		History: history, EpisodeID: p.EpisodeID, Attempt: p.Attempt, BudgetRunes: p.BudgetRunes,
		BeforeRunes: p.BeforeRunes, AfterRunes: estimateRunes(history),
	}))
	p.Phase, p.Step = "dispatched", 2
	records = append(records, recoveryRecord(t, store.KindContextRecovery, p))
	return sequenceRecoveryLog(records)
}

func sequenceRecoveryLog(records []store.Record) []store.Record {
	for i := range records {
		records[i].Seq = int64(i)
		if records[i].Kind == store.KindContextRecovery || records[i].Kind == store.KindHistoryReplaced {
			records[i].ID = fmt.Sprintf("recovery-%d", i)
		}
	}
	return records
}

func recoveryDone(t *testing.T, id string, truncated bool) store.Record {
	t.Helper()
	message := llm.AssistantMessage("answer")
	if truncated {
		message = llm.Message{Role: llm.RoleAssistant}
	}
	return mustRecord(t, store.KindAgentEvent, id, eventEnvelope{
		Type: "done", Version: eventEnvelopeV1,
		Payload: mustJSON(t, doneEventPayload{Message: message, Truncated: truncated}),
	})
}

func TestRecoveryReplayTransitions(t *testing.T) {
	log := recoveryLogFixture(t)
	for _, tc := range []struct {
		n       int
		phase   string
		attempt int
	}{
		{2, "pending", 0}, {3, "preparing", 1}, {4, "ready", 1}, {5, "dispatched", 1},
	} {
		t.Run(tc.phase, func(t *testing.T) {
			v, err := replayRecords(threadView{lastRound: -1, cfgSeq: -1}, log[:tc.n])
			if err != nil || v.recovery == nil || v.recovery.Phase != tc.phase || v.recovery.Attempt != tc.attempt {
				t.Fatalf("prefix = %+v, %v", v.recovery, err)
			}
			if tc.n >= 4 && messageString(v.history[0]) != strings.Repeat("s", 600) {
				t.Fatal("ready history was not replaced atomically")
			}
		})
	}
}

func TestRecoverySuccessAndTerminalClearing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		record store.Record
		phase  string
		reject bool
		active bool
	}{
		{"normal done", recoveryDone(t, "r-done", false), "dispatched", false, false},
		{"terminal text", mustRecord(t, store.KindRoundDeclared, "r-d0", roundDeclaredPayload{
			RunID: "r", Round: 0, Message: llm.AssistantMessage("continue"), Kind: terminalTextDeclaration,
		}), "dispatched", false, true},
		{"tool declaration", mustRecord(t, store.KindRoundDeclared, "r-d0", roundDeclaredPayload{
			RunID: "r", Round: 0, Message: toolCallMessage("tool"),
		}), "dispatched", false, true},
		{"truncated done", recoveryDone(t, "r-done", true), "dispatched", true, true},
		{"wrong done identity", recoveryDone(t, "a-done", false), "dispatched", true, true},
		{"wrong declaration identity", mustRecord(t, store.KindRoundDeclared, "a-d0", roundDeclaredPayload{
			RunID: "r", Round: 0, Message: llm.AssistantMessage("continue"), Kind: terminalTextDeclaration,
		}), "dispatched", true, true},
		{"success from preparing", recoveryDone(t, "r-done", false), "preparing", true, true},
		{"success from ready", recoveryDone(t, "r-done", false), "ready", true, true},
		{"cancelled", store.Record{Kind: store.KindRunCancelled, Schema: store.SchemaV2, ID: "r-cancel",
			Payload: mustJSON(t, runCancelledPayload{RunID: "r", Results: []llm.Message{}})}, "dispatched", false, false},
		{"legacy error", store.Record{Kind: store.KindError, Schema: store.SchemaV1, ID: "legacy"}, "dispatched", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := recoveryLogFixture(t)
			if tc.phase == "preparing" {
				log = log[:3]
			} else if tc.phase == "ready" {
				log = log[:4]
			}
			before, err := replayRecords(threadView{lastRound: -1, cfgSeq: -1}, log)
			if err != nil {
				t.Fatal(err)
			}
			tc.record.Seq = int64(len(log))
			after, err := replayRecords(before, []store.Record{tc.record})
			if tc.reject {
				if !errors.Is(err, ErrIncompatibleLog) || after.recovery == nil ||
					!reflect.DeepEqual(before.history, after.history) {
					t.Fatalf("invalid success changed recovery/history: %+v, %v", after, err)
				}
				return
			}
			if err != nil || after.recovery != nil || after.runActive != tc.active {
				t.Fatalf("terminal fold = %+v, %v", after, err)
			}
			if tc.name == "terminal text" && after.lastRound != 0 {
				t.Fatal("terminal_text skipped its round update")
			}
		})
	}
}

func TestRecoveryRejectsInvalidTransitions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		prefix int
		mutate func(*contextRecoveryPayload)
	}{
		{"third attempt", 3, func(p *contextRecoveryPayload) {
			p.Phase, p.Step, p.Attempt, p.BudgetRunes, p.Reason = "preparing", 2, 3, 500, "interrupted_attempt"
		}},
		{"nondecreasing budget", 3, func(p *contextRecoveryPayload) {
			p.Phase, p.Step, p.Attempt, p.Reason = "preparing", 2, 2, "interrupted_attempt"
		}},
		{"bare ready", 3, func(p *contextRecoveryPayload) { p.Phase, p.Step = "ready", 2 }},
		{"dispatch without replacement", 3, func(p *contextRecoveryPayload) { p.Phase, p.Step = "dispatched", 2 }},
		{"step gap", 3, func(p *contextRecoveryPayload) { p.Phase, p.Step, p.Reason = "blocked", 3, "no_progress" }},
		{"wrong run", 3, func(p *contextRecoveryPayload) { p.RunID = "other" }},
		{"wrong round", 3, func(p *contextRecoveryPayload) { p.Round = 1 }},
		{"wrong episode", 3, func(p *contextRecoveryPayload) { p.EpisodeID = "other" }},
		{"changed frozen model", 3, func(p *contextRecoveryPayload) { p.Model = "other" }},
		{"missing blocked reason", 3, func(p *contextRecoveryPayload) { p.Phase, p.Step = "blocked", 2 }},
		{"duplicate dispatch", 5, func(p *contextRecoveryPayload) { p.Phase, p.Step = "dispatched", 3 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := recoveryLogFixture(t)
			p, err := decodeContextRecovery(log[2])
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(&p)
			log = append(log[:tc.prefix], recoveryRecord(t, store.KindContextRecovery, p))
			_, err = replayRecords(threadView{lastRound: -1, cfgSeq: -1}, sequenceRecoveryLog(log))
			if !errors.Is(err, ErrIncompatibleLog) {
				t.Fatalf("invalid transition: %v", err)
			}
		})
	}
}

func TestRecoveryHistoryReplacementValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*historyReplacedPayload)
	}{
		{"mismatched episode", func(p *historyReplacedPayload) { p.EpisodeID = "other" }},
		{"mismatched attempt", func(p *historyReplacedPayload) { p.Attempt = 2 }},
		{"mismatched budget", func(p *historyReplacedPayload) { p.BudgetRunes-- }},
		{"false estimate", func(p *historyReplacedPayload) { p.AfterRunes-- }},
		{"system rewritten", func(p *historyReplacedPayload) {
			p.History = append([]llm.Message{llm.SystemMessage("injected")}, p.History...)
		}},
		{"unpaired tool", func(p *historyReplacedPayload) { p.History = []llm.Message{toolCallMessage("unpaired")} }},
		{"proactive during episode", func(p *historyReplacedPayload) {
			p.Reason, p.EpisodeID = "proactive_compaction", ""
			p.Attempt, p.BudgetRunes, p.BeforeRunes, p.AfterRunes = 0, 0, 0, 0
		}},
		{"anchor during episode", func(p *historyReplacedPayload) {
			p.Reason, p.EpisodeID = "legacy_anchor", ""
			p.Attempt, p.BudgetRunes, p.BeforeRunes, p.AfterRunes = 0, 0, 0, 0
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := recoveryLogFixture(t)
			p, err := decodeHistoryReplaced(log[3])
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(&p)
			log[3] = recoveryRecord(t, store.KindHistoryReplaced, p)
			v, err := replayRecords(threadView{lastRound: -1, cfgSeq: -1}, sequenceRecoveryLog(log[:4]))
			if !errors.Is(err, ErrIncompatibleLog) || v.recovery == nil || v.recovery.Phase != "preparing" {
				t.Fatalf("replacement changed recovery: %+v, %v", v.recovery, err)
			}
		})
	}
}

func TestRecoveryPendingRejectsNondecreasingOutboundBaseline(t *testing.T) {
	for _, delta := range []int{-1, 0, 1} {
		t.Run(fmt.Sprintf("delta=%d", delta), func(t *testing.T) {
			log := recoveryLogFixture(t)
			v, err := replayRecords(threadView{lastRound: -1, cfgSeq: -1}, log)
			if err != nil {
				t.Fatal(err)
			}
			p, err := decodeContextRecovery(log[4])
			if err != nil {
				t.Fatal(err)
			}
			p.Phase, p.Step, p.Reason = "pending", p.Step+1, "overflow"
			p.BeforeRunes = estimateRunes(v.history)
			p.RejectedOutboundRunes += delta
			r := recoveryRecord(t, store.KindContextRecovery, p)
			r.Seq = int64(len(log))
			_, err = replayRecords(v, []store.Record{r})
			if delta < 0 && err != nil {
				t.Fatalf("strictly smaller rejection rejected: %v", err)
			}
			if delta >= 0 && !errors.Is(err, ErrIncompatibleLog) {
				t.Fatalf("nondecreasing rejection accepted: %v", err)
			}
		})
	}
}

func TestRecoveryPreparingEnforcesThreeQuarterCeiling(t *testing.T) {
	for _, prefix := range []int{2, 3, 5} {
		for _, extra := range []int{0, 1} {
			t.Run(fmt.Sprintf("prefix=%d/above=%d", prefix, extra), func(t *testing.T) {
				log := recoveryLogFixture(t)[:prefix]
				v, err := replayRecords(threadView{lastRound: -1, cfgSeq: -1}, log)
				if err != nil {
					t.Fatal(err)
				}
				p, err := decodeContextRecovery(log[len(log)-1])
				if err != nil {
					t.Fatal(err)
				}
				p.Phase, p.Step, p.Attempt = "preparing", p.Step+1, p.Attempt+1
				p.BeforeRunes = estimateRunes(v.history)
				n := p.BeforeRunes
				if p.BudgetRunes > 0 && p.BudgetRunes < n {
					n = p.BudgetRunes
				}
				p.BudgetRunes = n*3/4 + extra
				if prefix != 2 {
					p.Reason = "interrupted_attempt"
				}
				r := recoveryRecord(t, store.KindContextRecovery, p)
				r.Seq = int64(len(log))
				_, err = replayRecords(v, []store.Record{r})
				if extra == 0 && err != nil {
					t.Fatalf("ceiling rejected: %v", err)
				}
				if extra != 0 && !errors.Is(err, ErrIncompatibleLog) {
					t.Fatalf("budget above ceiling accepted: %v", err)
				}
			})
		}
	}
}

func TestRecoveryActiveEpisodeQueuesButNeverConsumesInput(t *testing.T) {
	for _, prefix := range []int{2, 3, 4, 5} {
		t.Run(fmt.Sprintf("prefix=%d", prefix), func(t *testing.T) {
			log := recoveryLogFixture(t)[:prefix]
			queued := mustRecord(t, store.KindInputQueued, "queued", inputQueuedPayload{Input: "steering"})
			queued.Schema = store.SchemaV2
			log = sequenceRecoveryLog(append(log, queued))
			v, err := replayRecords(threadView{lastRound: -1, cfgSeq: -1}, log)
			if err != nil || len(v.queue) != 1 {
				t.Fatalf("queue should remain legal: %+v, %v", v, err)
			}
			drain := mustRecord(t, store.KindInputConsumed, "drain", inputConsumedPayload{Inputs: []consumedInput{{ID: "queued", Input: "steering"}}})
			drain.Schema, drain.Seq = store.SchemaV2, int64(len(log))
			got, err := replayRecords(v, []store.Record{drain})
			if !errors.Is(err, ErrIncompatibleLog) || !reflect.DeepEqual(got.history, v.history) ||
				!reflect.DeepEqual(got.queue, v.queue) {
				t.Fatalf("active recovery consumed steering: %+v, %v", got, err)
			}
		})
	}
}
