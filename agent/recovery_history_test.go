package agent

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/agenttest"
	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

func TestRecoveryDoesNotReexecutePersistedTools(t *testing.T) {
	reg := tool.NewRegistry()
	executions := 0
	reg.MustRegister(mutatorTool("work", func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		executions++
		return tool.NewTextResult("saved"), nil
	}))
	input := strings.Repeat("x", 2000)
	start := []llm.Message{llm.UserMessage(input)}
	oldCall := llm.ToolUseBlock{Type: "tool_use", ID: "old", Name: "work", Input: json.RawMessage(`{}`)}
	newCall := llm.ToolUseBlock{Type: "tool_use", ID: "new", Name: "work", Input: json.RawMessage(`{}`)}
	oldResult := llm.ToolResultMessage("old", tool.NewTextResult("saved"))
	newResult := llm.ToolResultMessage("new", tool.NewTextResult("saved"))
	before := append(deepCopyMessages(start), llm.AssistantToolCallMessage(oldCall), oldResult)
	after := []llm.Message{llm.UserMessage("summary"), llm.AssistantToolCallMessage(oldCall), oldResult}
	final := append(deepCopyMessages(after), llm.AssistantToolCallMessage(newCall), newResult)
	exchanges := []agenttest.Exchange{
		recoveryExchange(start, nil, nil, llm.ToolCallStartChunk{ID: "old", Name: "work"}, llm.ToolCallArgsChunk{Delta: `{}`}),
		recoveryExchange(before, overflowTestError(), nil),
		recoveryExchange(after, nil, nil, llm.ToolCallStartChunk{ID: "new", Name: "work"}, llm.ToolCallArgsChunk{Delta: `{}`}),
		recoveryExchange(final, nil, nil, llm.TextDeltaChunk{Text: "done"}),
	}
	for i := range exchanges {
		exchanges[i].Request.Tools = reg.List()
	}
	p := agenttest.NewScriptedProvider(exchanges...)
	a := New(p, reg, WithStore(store.NewMemory()), WithLogger(discardLogger()), WithMaxIter(3),
		WithCompactor(recoveryCompactor(after)))
	result, err := a.Run(t.Context(), input, WithThread("recovery"))
	if err != nil {
		t.Fatal(err)
	}
	if executions != 2 || result.ToolCalls != 2 {
		t.Fatalf("old tool executed again: executions %d, calls %d", executions, result.ToolCalls)
	}
	if err := p.Verify(); err != nil {
		t.Fatal(err)
	}
	v := recoveryView(t, a)
	if v.recovery != nil || !reflect.DeepEqual(v.history, result.History) {
		t.Fatalf("tool declaration did not clear recovery: %+v", v)
	}
}

func TestRecoveryCanonicalHistoryDoesNotDependOnCheckpoint(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "proactive", true: "legacy anchor then overflow"}[legacy], func(t *testing.T) {
			st := store.NewMemory()
			original := strings.Repeat("x", 10000)
			legacyHistory := []llm.Message{llm.UserMessage(strings.Repeat("l", 2000))}
			short := []llm.Message{llm.UserMessage("summary")}
			var p *agenttest.ScriptedProvider
			if legacy {
				records := []store.Record{
					mustRecord(t, store.KindRunStarted, "r-start", runStartedPayload{RunID: "r", Input: original}),
					mustRecord(t, store.KindCheckpoint, "legacy", agentCheckpoint{
						CodecVersion: store.SchemaV3, History: legacyHistory, RunActive: true, RunID: "r", LastRound: -1,
					}),
				}
				records[1].Schema = store.SchemaV3
				if _, err := st.Append(t.Context(), "recovery", 0, records...); err != nil {
					t.Fatal(err)
				}
				p = agenttest.NewScriptedProvider(recoveryExchange(legacyHistory, overflowTestError(), nil),
					recoveryExchange(short, nil, nil, llm.TextDeltaChunk{Text: "done"}))
			} else {
				p = agenttest.NewScriptedProvider(recoveryExchange(short, nil, nil, llm.TextDeltaChunk{Text: "done"}))
			}
			a := New(p, tool.NewRegistry(), WithStore(st), WithLogger(discardLogger()), WithCompactor(recoveryCompactor(short)))
			var result *RunResult
			var err error
			if legacy {
				result, err = a.ResumeThread(t.Context(), "recovery")
			} else {
				result, err = a.Run(t.Context(), original, WithThread("recovery"))
			}
			if err != nil {
				t.Fatal(err)
			}
			log, err := st.History(t.Context(), "recovery", 0)
			if err != nil {
				t.Fatal(err)
			}
			v, err := replayRecords(threadView{lastRound: -1, cfgSeq: -1}, log)
			if err != nil || !reflect.DeepEqual(v.history, result.History) || v.recovery != nil {
				t.Fatalf("full replay lost canonical replacement: %+v, %v", v, err)
			}
			var reasons []string
			for _, record := range log {
				if record.Kind == store.KindHistoryReplaced {
					payload, err := decodeHistoryReplaced(record)
					if err != nil {
						t.Fatal(err)
					}
					reasons = append(reasons, payload.Reason)
				}
				if legacy && record.Kind == store.KindContextRecovery && len(reasons) == 0 {
					t.Fatal("recovery marker preceded legacy anchor")
				}
			}
			want := []string{"proactive_compaction"}
			if legacy {
				want = []string{"legacy_anchor", "overflow_compaction"}
			}
			if !reflect.DeepEqual(reasons, want) {
				t.Fatalf("replacement reasons %v want %v", reasons, want)
			}
			if err := p.Verify(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRecoveryLegacyCheckpointWriteAnchorsBeforeV4(t *testing.T) {
	st := store.NewMemory()
	history := []llm.Message{llm.UserMessage("legacy summary")}
	records := []store.Record{
		mustRecord(t, store.KindRunStarted, "r-start", runStartedPayload{RunID: "r", Input: "original"}),
		mustRecord(t, store.KindCheckpoint, "legacy", agentCheckpoint{CodecVersion: store.SchemaV3,
			History: history, RunActive: true, RunID: "r", LastRound: -1}),
	}
	records[1].Schema = store.SchemaV3
	if _, err := st.Append(t.Context(), "recovery", 0, records...); err != nil {
		t.Fatal(err)
	}
	a := New(nil, nil, WithStore(st))
	p, h, err := a.openSession(t.Context(), st, "recovery", "", true, false)
	if err != nil {
		t.Fatal(err)
	}
	defer p.release()
	if err := p.checkpoint(t.Context(), h); err != nil {
		t.Fatal(err)
	}
	log, err := st.History(t.Context(), "recovery", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(log) != 4 || log[2].Kind != store.KindHistoryReplaced || log[3].Schema != store.SchemaV4 {
		t.Fatalf("v4 checkpoint preceded anchor: %+v", log)
	}
	v, err := replayRecords(threadView{lastRound: -1, cfgSeq: -1}, log)
	if err != nil || !reflect.DeepEqual(v.history, history) {
		t.Fatalf("legacy view not canonical: %+v, %v", v.history, err)
	}
}
