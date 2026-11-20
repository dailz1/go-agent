package agent

import (
	"bytes"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

func measurementCheckpoint(t *testing.T, v threadView) store.Record {
	t.Helper()
	r := mustRecord(t, store.KindCheckpoint, "measurement-checkpoint", agentCheckpoint{
		CodecVersion: store.SchemaV5, History: seededHistory(v.system, v.history), System: v.system,
		RunActive: v.runActive, RunID: v.runID, LastRound: v.lastRound,
		Config: &v.cfg, Queue: v.queue, ConfigSeq: &v.cfgSeq, Recovery: v.recovery, ContextAnchor: v.anchor,
	})
	r.Schema, r.Seq = store.SchemaV5, v.head
	return r
}

func TestMeasurementCheckpointCanonicalParity(t *testing.T) {
	for _, mode := range []string{"no_anchor", "restored_anchor", "open_declaration_partial_results"} {
		t.Run(mode, func(t *testing.T) {
			usage := &llm.Usage{InputTokens: 2000}
			msg := llm.AssistantMessage("measured")
			if mode == "no_anchor" {
				usage = nil
			}
			if mode == "open_declaration_partial_results" {
				msg = toolCallMessage("one", "two")
			}
			log := []store.Record{
				mustRecord(t, store.KindRunStarted, "r-start", runStartedPayload{RunID: "r", Input: "q", SystemPrompt: "frozen"}),
				measurementRecord(t, 0, msg, usage, 3),
			}
			if mode == "open_declaration_partial_results" {
				log = append(log, mustRecord(t, store.KindRoundResult, "r-r0-0", roundResultPayload{
					RunID: "r", Round: 0, Index: 0, Result: llm.ToolResultMessage("one", tool.NewTextResult("actual")),
				}))
			}
			log = sequenceRecoveryLog(log)
			v := measurementView(t, log)
			for _, backend := range []string{"memory", "jsonl"} {
				t.Run(backend, func(t *testing.T) {
					st, reopen := measurementBackend(t, backend)
					cp := measurementCheckpoint(t, v)
					if _, err := st.Append(t.Context(), "thread", 0, append(log, cp)...); err != nil {
						t.Fatal(err)
					}
					a := New(nil, nil, WithStore(reopen()))
					replayed, err := a.replayThread(t.Context(), "thread")
					if err != nil {
						t.Fatal(err)
					}
					if got, want := measurementEstimate(replayed), measurementEstimate(v); got != want {
						t.Fatalf("checkpoint/full-log estimates = %d/%d", got, want)
					}
				})
			}
		})
	}
}

func TestMeasurementCorruptCheckpointProjectionFallsBack(t *testing.T) {
	log := sequenceRecoveryLog([]store.Record{
		mustRecord(t, store.KindRunStarted, "r-start", runStartedPayload{RunID: "r", Input: "q"}),
		measurementRecord(t, 0, llm.AssistantMessage("measured"), &llm.Usage{InputTokens: 2000}, 2),
	})
	v := measurementView(t, log)
	for _, tc := range []struct {
		key, value string
	}{
		{"input_tokens", "3000"}, {"output_tokens", "99"},
		{"prefix_messages", "1"}, {"source_seq", "0"}, {"source_record_id", `"r-start"`},
		{"provider", `"wrong"`}, {"model", `"wrong"`},
		{"request_fingerprint", `"` + strings.Repeat("0", 64) + `"`},
		{"input_tokens", ""}, {"source_seq", ""}, {"output_tokens", ""},
		{"context_anchor", "null"}, {"context_anchor", ""},
	} {
		t.Run(tc.key+"/"+tc.value, func(t *testing.T) {
			cp := measurementCheckpoint(t, v)
			var value []byte
			if tc.value != "" {
				value = []byte(tc.value)
			}
			if tc.key == "context_anchor" {
				cp = mutateRecoveryJSON(t, cp, tc.key, value)
			} else {
				raw := mutateRecoveryJSON(t, store.Record{Payload: mustJSON(t, v.anchor)}, tc.key, value)
				cp = mutateRecoveryJSON(t, cp, "context_anchor", raw.Payload)
			}
			a := New(nil, nil, WithStore(&scriptStore{records: append(log, cp)}))
			replayed, err := a.replayThread(t.Context(), "thread")
			if err != nil || measurementEstimate(replayed) != 2000 {
				t.Fatalf("corrupt checkpoint overrode canonical usage: %d, %v", measurementEstimate(replayed), err)
			}
		})
	}
}

func TestMeasurementOldCheckpointCannotHideSchemaV5(t *testing.T) {
	log := sequenceRecoveryLog([]store.Record{
		mustRecord(t, store.KindRunStarted, "r-start", runStartedPayload{RunID: "r", Input: "q"}),
		measurementRecord(t, 0, llm.AssistantMessage("measured"), &llm.Usage{InputTokens: 2000}, 2),
	})
	v := measurementView(t, log)
	for _, version := range []int{store.SchemaV1, store.SchemaV2, store.SchemaV3, store.SchemaV4} {
		t.Run(jsonNumber(version), func(t *testing.T) {
			codec := version
			if version == 1 {
				codec = 0
			}
			payload := agentCheckpoint{
				CodecVersion: codec, History: []llm.Message{llm.UserMessage("stale")},
				RunActive: true, RunID: "r", LastRound: 0,
			}
			if version == 4 {
				payload.ConfigSeq = &v.cfgSeq
			}
			cp := mustRecord(t, store.KindCheckpoint, "old", payload)
			cp.Schema, cp.Seq = version, int64(len(log))
			a := New(nil, nil, WithStore(&scriptStore{records: append(log, cp)}))
			replayed, err := a.replayThread(t.Context(), "thread")
			if err != nil || measurementEstimate(replayed) != 2000 || historyContainsText(replayed.history, "stale") {
				t.Fatalf("old checkpoint hid schema 5: %+v, %v", replayed, err)
			}
		})
	}
}

func TestMeasurementLegacyV3AnchoredBeforeFirstNewResponse(t *testing.T) {
	for _, backend := range []string{"memory", "jsonl"} {
		t.Run(backend, func(t *testing.T) {
			st, reopen := measurementBackend(t, backend)
			log := []store.Record{
				mustRecord(t, store.KindRunStarted, "r-start", runStartedPayload{RunID: "r", Input: "discarded original"}),
				mustRecord(t, store.KindCheckpoint, "legacy", agentCheckpoint{
					CodecVersion: 3, History: []llm.Message{llm.UserMessage("legacy summary")},
					RunActive: true, RunID: "r", LastRound: -1,
				}),
			}
			log[1].Schema = 3
			if _, err := st.Append(t.Context(), "thread", 0, log...); err != nil {
				t.Fatal(err)
			}
			var buf bytes.Buffer
			a := measurementTestAgent(reopen(), measurementRegistry(0),
				[][]llm.Chunk{contextChunks("done", &llm.Usage{InputTokens: 2000})}, &buf)
			if _, err := a.ResumeThread(t.Context(), "thread"); err != nil {
				t.Fatal(err)
			}
			records, err := a.store.History(t.Context(), "thread", 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(records) != 4 || records[2].Kind != store.KindHistoryReplaced ||
				records[3].Schema != store.SchemaV5 {
				t.Fatalf("legacy_anchor did not precede response: %+v", records)
			}
			v := measurementView(t, records)
			if historyContainsText(v.history, "discarded original") || !historyContainsText(v.history, "legacy summary") ||
				measurementEstimate(v) != 2000 {
				t.Fatalf("legacy migration lost canonical history: %+v", v)
			}
		})
	}
}
