package agent

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/dailz1/go-agent/agenttest"
	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

func recoveryCheckpointRecord(t *testing.T, v threadView) store.Record {
	t.Helper()
	return recoveryRecord(t, store.KindCheckpoint, agentCheckpoint{
		CodecVersion: store.SchemaV4, History: seededHistory(v.system, v.history), System: v.system,
		RunActive: v.runActive, RunID: v.runID, LastRound: v.lastRound,
		Config: &v.cfg, Queue: v.queue, ConfigSeq: &v.cfgSeq, Recovery: v.recovery,
	})
}

func TestRecoveryCheckpointRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name   string
		length int
		note   bool
	}{
		{"pending", 2, false}, {"preparing", 3, false}, {"ready", 4, false},
		{"dispatched", 5, false}, {"provider failure note", 5, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := recoveryLogFixture(t)[:tc.length]
			if tc.note {
				p, err := decodeContextRecovery(log[4])
				if err != nil {
					t.Fatal(err)
				}
				p.Step, p.Reason = 3, "provider_error"
				log = sequenceRecoveryLog(append(log, recoveryRecord(t, store.KindContextRecovery, p)))
			}
			v, err := replayRecords(threadView{lastRound: -1, cfgSeq: -1}, log)
			if err != nil {
				t.Fatal(err)
			}
			cp := recoveryCheckpointRecord(t, v)
			cp.Seq = int64(len(log))
			got, err := decodeCheckpoint(cp)
			if err != nil || got == nil || !reflect.DeepEqual(got.Recovery, v.recovery) ||
				got.ConfigSeq == nil || *got.ConfigSeq != -1 {
				t.Fatalf("checkpoint round trip = %+v, %v", got, err)
			}
			a := New(NewMockProvider(), tool.NewRegistry(), WithStore(&scriptStore{records: append(log, cp)}))
			replayed, err := a.replayThread(t.Context(), "t")
			if err != nil || !reflect.DeepEqual(replayed.recovery, v.recovery) || replayed.cfgSeq != -1 ||
				!reflect.DeepEqual(replayed.history, v.history) {
				t.Fatalf("checkpoint adoption = %+v, %v", replayed, err)
			}
		})
	}
}

func TestRecoveryCheckpointPresenceAndVersions(t *testing.T) {
	log := recoveryLogFixture(t)
	v, err := replayRecords(threadView{lastRound: -1, cfgSeq: -1}, log[:4])
	if err != nil {
		t.Fatal(err)
	}
	cp := recoveryCheckpointRecord(t, v)
	for _, tc := range []struct {
		name, key, value string
		incompatible     bool
	}{
		{"missing config sequence", "config_seq", "", false},
		{"null config sequence", "config_seq", "null", false},
		{"invalid config sequence", "config_seq", "-2", false},
		{"missing recovery", "recovery", "", false},
		{"old codec on v4 schema", "codec_version", "3", false},
		{"missing codec on v4 schema", "codec_version", "", false},
		{"null codec on v4 schema", "codec_version", "null", false},
		{"future codec", "codec_version", "6", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var value json.RawMessage
			if tc.value != "" {
				value = json.RawMessage(tc.value)
			}
			got, err := decodeCheckpoint(mutateRecoveryJSON(t, cp, tc.key, value))
			if got != nil || (tc.incompatible && !errors.Is(err, ErrIncompatibleLog)) || (!tc.incompatible && err != nil) {
				t.Fatalf("invalid checkpoint = %+v, %v", got, err)
			}
		})
	}
	for _, key := range []string{"round", "step", "provider", "model", "config_seq", "attempt", "budget_runes", "before_runes", "rejected_outbound_runes", "code", "reason"} {
		t.Run("recovery missing "+key, func(t *testing.T) {
			rp := store.Record{Payload: mustJSON(t, v.recovery)}
			rp = mutateRecoveryJSON(t, rp, key, nil)
			got, err := decodeCheckpoint(mutateRecoveryJSON(t, cp, "recovery", rp.Payload))
			if got != nil || err != nil {
				t.Fatalf("incomplete recovery snapshot should fall back: %+v, %v", got, err)
			}
		})
	}
	for _, schema := range []int{1, 2, 3, 5, 6} {
		t.Run("schema "+jsonNumber(schema), func(t *testing.T) {
			bad := cp
			bad.Schema = schema
			got, err := decodeCheckpoint(bad)
			if got != nil || (schema == 6 && !errors.Is(err, ErrIncompatibleLog)) || (schema < 6 && err != nil) {
				t.Fatalf("schema/codec mismatch: %+v, %v", got, err)
			}
		})
	}
}

func TestRecoveryCheckpointEntireProjectionMatchesCanonicalPrefix(t *testing.T) {
	for _, field := range []string{"history", "system", "config", "queue", "run_id", "last_round", "schema"} {
		t.Run(field, func(t *testing.T) {
			log := replacementIndexLog(t, 1)
			v, err := replayRecords(threadView{lastRound: -1, cfgSeq: -1}, log)
			if err != nil {
				t.Fatal(err)
			}
			cp := recoveryCheckpointRecord(t, v)
			cp.Seq = int64(len(log))
			switch field {
			case "history":
				cp = mutateRecoveryJSON(t, cp, field, mustJSON(t, []llm.Message{llm.UserMessage("stale")}))
			case "system":
				cp = mutateRecoveryJSON(t, cp, "system", mustJSON(t, "stale system"))
				cp = mutateRecoveryJSON(t, cp, "history", mustJSON(t, seededHistory("stale system", v.history)))
			case "config":
				cp = mutateRecoveryJSON(t, cp, field, mustJSON(t, configState{Model: strptr("stale")}))
			case "queue":
				cp = mutateRecoveryJSON(t, cp, field, mustJSON(t, []queueEntry{{ID: "consumed", Input: "stale"}}))
			case "run_id":
				cp = mutateRecoveryJSON(t, cp, field, mustJSON(t, "stale-run"))
			case "last_round":
				cp = mutateRecoveryJSON(t, cp, field, mustJSON(t, 4))
			case "schema":
				cp.Schema = store.SchemaV3
			}
			if field != "schema" {
				if decoded, err := decodeCheckpoint(cp); err != nil || decoded == nil {
					t.Fatalf("fixture is not structurally valid: %v", err)
				}
			}
			a := New(NewMockProvider(), tool.NewRegistry(), WithStore(&scriptStore{records: append(log, cp)}))
			got, err := a.replayThread(t.Context(), "t")
			if err != nil || !reflect.DeepEqual(got.history, v.history) || got.system != v.system ||
				!reflect.DeepEqual(got.cfg, v.cfg) || len(got.queue) != 0 ||
				got.runID != v.runID || got.lastRound != v.lastRound || got.recovery != nil {
				t.Fatalf("snapshot overrode canonical prefix: %+v, %v", got, err)
			}
		})
	}
}

func TestRecoveryDamagedCheckpointCodecCannotPoisonFullReplay(t *testing.T) {
	for _, codec := range []string{"", "null", "0", "3", `"invalid"`} {
		for _, newer := range []bool{false, true} {
			t.Run("codec="+codec+"/newer="+map[bool]string{false: "no", true: "yes"}[newer], func(t *testing.T) {
				log := recoveryLogFixture(t)[:4]
				v, err := replayRecords(threadView{lastRound: -1, cfgSeq: -1}, log)
				if err != nil {
					t.Fatal(err)
				}
				cp := recoveryCheckpointRecord(t, v)
				cp.ID, cp.Seq = "damaged", int64(len(log))
				var value json.RawMessage
				if codec != "" {
					value = json.RawMessage(codec)
				}
				cp = mutateRecoveryJSON(t, cp, "codec_version", value)
				log = append(log, cp)
				if newer {
					cp = recoveryCheckpointRecord(t, v)
					cp.ID, cp.Seq = "newer", int64(len(log))
					log = append(log, cp)
				}
				a := New(NewMockProvider(), tool.NewRegistry(), WithStore(&scriptStore{records: log}))
				got, err := a.replayThread(t.Context(), "t")
				if err != nil || !reflect.DeepEqual(got.recovery, v.recovery) || !reflect.DeepEqual(got.history, v.history) {
					t.Fatalf("damaged optional codec poisoned replay: %+v, %v", got, err)
				}
			})
		}
	}
}

func TestRecoveryCheckpointStaleProjectionFallsBackToCanonicalPrefix(t *testing.T) {
	for _, stale := range []string{"recovery", "config_seq"} {
		t.Run(stale, func(t *testing.T) {
			log := recoveryLogFixture(t)
			p, err := decodeContextRecovery(log[4])
			if err != nil {
				t.Fatal(err)
			}
			v, err := replayRecords(threadView{lastRound: -1, cfgSeq: -1}, log)
			if err != nil {
				t.Fatal(err)
			}
			p.Phase, p.Step, p.Attempt, p.BudgetRunes = "preparing", 3, 2, 400
			p.Reason, p.BeforeRunes = "interrupted_attempt", estimateRunes(v.history)
			log = append(log, recoveryRecord(t, store.KindContextRecovery, p))
			short := []llm.Message{llm.UserMessage("short")}
			log = append(log, recoveryRecord(t, store.KindHistoryReplaced, historyReplacedPayload{
				RunID: "r", OperationID: "second", Reason: "overflow_compaction", History: short,
				EpisodeID: p.EpisodeID, Attempt: 2, BudgetRunes: 400,
				BeforeRunes: p.BeforeRunes, AfterRunes: estimateRunes(short),
			}))
			p.Phase, p.Step, p.Reason = "dispatched", 4, "request"
			log = append(log, recoveryRecord(t, store.KindContextRecovery, p))
			change := mustRecord(t, store.KindConfigChanged, "thinking", configChangedPayload{Thinking: strptr("high")})
			change.Schema = store.SchemaV2
			log = sequenceRecoveryLog(append(log, change))
			v, err = replayRecords(threadView{lastRound: -1, cfgSeq: -1}, log)
			if err != nil {
				t.Fatal(err)
			}
			cp := recoveryCheckpointRecord(t, v)
			cp.ID, cp.Seq = "stale", int64(len(log))
			value := json.RawMessage("null")
			if stale == "config_seq" {
				value = json.RawMessage("-1")
			}
			cp = mutateRecoveryJSON(t, cp, stale, value)
			if decoded, err := decodeCheckpoint(cp); decoded == nil || err != nil {
				t.Fatalf("fixture must be structurally valid: %v", err)
			}
			provider := agenttest.NewScriptedProvider()
			a := New(recoveryNamedProvider{Provider: provider, name: "a"}, tool.NewRegistry(),
				WithStore(&scriptStore{records: append(log, cp)}), WithLogger(discardLogger()))
			got, err := a.replayThread(t.Context(), "t")
			if err != nil || !reflect.DeepEqual(got.recovery, v.recovery) || got.cfgSeq != v.cfgSeq {
				t.Fatalf("snapshot overrode canonical projection: recovery %+v, cfg %d, err %v", got.recovery, got.cfgSeq, err)
			}
			_, err = a.ResumeThread(t.Context(), "t")
			requireRecoveryError(t, err, "attempts_exhausted", 2)
			if err := provider.Verify(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRecoveryLegacyCheckpointCannotHideV4(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(map[bool]string{false: "v3 hides recovery", true: "corrupt v4 falls back"}[corrupt], func(t *testing.T) {
			log := recoveryLogFixture(t)[:4]
			cp := mustRecord(t, store.KindCheckpoint, "legacy-cp", agentCheckpoint{
				CodecVersion: store.SchemaV3, History: []llm.Message{llm.UserMessage("wrong hidden view")},
				RunActive: true, RunID: "r", LastRound: -1,
			})
			cp.Schema, cp.Seq = store.SchemaV3, int64(len(log))
			if corrupt {
				cp.Schema = store.SchemaV4
				cp.Payload = []byte(`{"codec_version":4,"history":"corrupt"}`)
			}
			a := New(NewMockProvider(), tool.NewRegistry(), WithStore(&scriptStore{records: append(log, cp)}))
			v, err := a.replayThread(t.Context(), "t")
			if err != nil || v.recovery == nil || v.recovery.Phase != "ready" ||
				len(v.history) != 1 || len(messageString(v.history[0])) != 600 {
				t.Fatalf("fallback lost canonical recovery: %+v, %v", v, err)
			}
		})
	}
}

func TestRecoveryConfigSequenceZero(t *testing.T) {
	for _, version := range []int{0, store.SchemaV3, store.SchemaV4} {
		t.Run("checkpoint "+jsonNumber(version), func(t *testing.T) {
			model := "model-at-zero"
			config := mustRecord(t, store.KindConfigChanged, "cfg-zero", configChangedPayload{Model: &model})
			config.Schema = store.SchemaV2
			log := sequenceRecoveryLog([]store.Record{
				config, mustRecord(t, store.KindRunStarted, "r-start", runStartedPayload{RunID: "r", Input: "original"}),
			})
			if version != 0 {
				cp := agentCheckpoint{CodecVersion: version, RunActive: true, RunID: "r", LastRound: -1,
					History: []llm.Message{llm.UserMessage("snapshot")}, Config: &configState{Model: &model}}
				if version == store.SchemaV4 {
					zero := int64(0)
					cp.ConfigSeq = &zero
				}
				r := mustRecord(t, store.KindCheckpoint, "cp", cp)
				r.Schema, r.Seq = version, 2
				log = append(log, r)
			}
			a := New(NewMockProvider(), tool.NewRegistry(), WithStore(&scriptStore{records: log}))
			v, err := a.replayThread(t.Context(), "t")
			if err != nil || v.cfgSeq != 0 || v.cfg.Model == nil || *v.cfg.Model != model {
				t.Fatalf("seq zero lost: %+v, %v", v, err)
			}
			want := "original"
			if version == store.SchemaV3 {
				want = "snapshot"
			}
			if messageString(v.history[0]) != want {
				t.Fatalf("checkpoint history = %q, want %q", messageString(v.history[0]), want)
			}
		})
	}
	a := New(NewMockProvider(), tool.NewRegistry(), WithStore(store.NewMemory()))
	v, err := a.replayThread(t.Context(), "empty")
	if err != nil || v.cfgSeq != -1 {
		t.Fatalf("no config must be -1: %d, %v", v.cfgSeq, err)
	}
}

func TestRecoveryCheckpointWriterCarriesLiveProjection(t *testing.T) {
	st := store.NewMemory()
	log := recoveryLogFixture(t)[:4]
	if _, err := st.Append(t.Context(), "t", 0, log...); err != nil {
		t.Fatal(err)
	}
	a := New(NewMockProvider(), tool.NewRegistry(), WithStore(st))
	v, err := a.replayThread(t.Context(), "t")
	if err != nil {
		t.Fatal(err)
	}
	p := &persistence{a: a, thread: "t", runID: "r", head: v.head, lastRound: -1, open: -1,
		cfgRead: v.head, cfgSeq: v.cfgSeq, recovery: v.recovery}
	if err := p.checkpoint(t.Context(), v.history); err != nil {
		t.Fatal(err)
	}
	saved, err := a.replayThread(t.Context(), "t")
	if err != nil || !reflect.DeepEqual(saved.recovery, v.recovery) || saved.cfgSeq != -1 {
		t.Fatalf("live checkpoint projection: %+v, %v", saved, err)
	}
}
