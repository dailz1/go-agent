package agent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

type replayFixture struct {
	runID string
	recs  []store.Record
}

func newReplayFixture() *replayFixture {
	return &replayFixture{runID: "run"}
}

func (f *replayFixture) add(kind, id string, payload any) {
	rec, err := encodeRecord(kind, id, payload)
	if err != nil {
		panic(err)
	}
	f.recs = append(f.recs, rec)
}

func (f *replayFixture) started() {
	f.add(store.KindRunStarted, recordID(f.runID, "-start"),
		runStartedPayload{RunID: f.runID, Input: "go", SystemPrompt: ""})
}

func (f *replayFixture) declared(round int, ids ...string) {
	f.add(store.KindRoundDeclared, recordID(f.runID, "-d"+itoa(round)),
		roundDeclaredPayload{RunID: f.runID, Round: round, Message: toolCallMessage(ids...)})
}

func (f *replayFixture) committed(round int, results []llm.Message) {
	f.add(store.KindRoundCommitted, recordID(f.runID, "-c"+itoa(round)),
		roundCommittedPayload{RunID: f.runID, Round: round, Results: results})
}

func (f *replayFixture) result(round, index int, callID, content string, isError bool) {
	f.add(store.KindRoundResult, recordID(f.runID, "-r"+itoa(round)+"-"+itoa(index)),
		roundResultPayload{RunID: f.runID, Round: round, Index: index,
			Result: resultMessage(callID, content, isError)})
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}

func resultMessage(callID, content string, isError bool) llm.Message {
	status := tool.ResultStatus("")
	if isError {
		status = tool.ResultError
	}
	return llm.ToolResultMessage(callID, &tool.ToolResult{Content: content, Status: status})
}

func unknownBatch(ids ...string) []llm.Message {
	return unknownResults(toolCallMessage(ids...))
}

func replayAgent(records []store.Record) *Agent {
	return New(NewMockStreamingProvider(nil), tool.NewRegistry(),
		WithStore(&scriptStore{records: records}), WithLogger(discardLogger()))
}

func TestReplayRoundResultCoverage(t *testing.T) {
	for _, tc := range []struct {
		name      string
		build     func(f *replayFixture)
		wantOpen  bool
		wantCount int
	}{
		{name: "none", build: func(f *replayFixture) { f.started(); f.declared(0, "c1", "c2") },
			wantOpen: true, wantCount: 0},
		{name: "first_only", build: func(f *replayFixture) {
			f.started()
			f.declared(0, "c1", "c2")
			f.result(0, 0, "c1", "real", false)
		}, wantOpen: true, wantCount: 1},
		{name: "all_without_close", build: func(f *replayFixture) {
			f.started()
			f.declared(0, "c1", "c2")
			f.result(0, 0, "c1", "real", false)
			f.result(0, 1, "c2", "soft", true)
		}, wantOpen: true, wantCount: 2},
		{name: "full_with_close", build: func(f *replayFixture) {
			f.started()
			f.declared(0, "c1", "c2")
			f.result(0, 0, "c1", "real", false)
			f.result(0, 1, "c2", "soft", true)
			f.committed(0, []llm.Message{resultMessage("c1", "real", false), resultMessage("c2", "soft", true)})
		}, wantOpen: false, wantCount: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReplayFixture()
			tc.build(f)
			v, err := replayAgent(f.recs).replayThread(context.Background(), "t")
			if err != nil {
				t.Fatalf("replay: %v", err)
			}
			if (v.open != nil) != tc.wantOpen {
				t.Fatalf("open = %v, want %v", v.open != nil, tc.wantOpen)
			}
			if v.open != nil && len(v.open.results) != tc.wantCount {
				t.Fatalf("recorded prefix = %d, want %d", len(v.open.results), tc.wantCount)
			}
			if v.open != nil && len(v.open.results) > 0 && v.open.results[0].Role != llm.RoleTool {
				t.Fatalf("prefix[0] = %+v, want a tool message", v.open.results[0])
			}
		})
	}
}

func TestReplayRejectsInvalidRoundResult(t *testing.T) {
	base := func(f *replayFixture) {
		f.started()
		f.declared(0, "c1", "c2")
		f.result(0, 0, "c1", "real", false)
	}
	for _, tc := range []struct {
		name  string
		build func(f *replayFixture)
	}{
		{name: "no_active_run", build: func(f *replayFixture) { f.result(0, 0, "c1", "x", false) }},
		{name: "wrong_run", build: func(f *replayFixture) {
			f.started()
			f.declared(0, "c1", "c2")
			f.add(store.KindRoundResult, "other-r0-0",
				roundResultPayload{RunID: "other", Round: 0, Index: 0, Result: resultMessage("c1", "x", false)})
		}},
		{name: "wrong_round", build: func(f *replayFixture) {
			f.started()
			f.declared(0, "c1", "c2")
			f.add(store.KindRoundResult, "run-r1-0",
				roundResultPayload{RunID: f.runID, Round: 1, Index: 0, Result: resultMessage("c1", "x", false)})
		}},
		{name: "wrong_record_id", build: func(f *replayFixture) {
			f.started()
			f.declared(0, "c1", "c2")
			f.add(store.KindRoundResult, "bogus",
				roundResultPayload{RunID: f.runID, Round: 0, Index: 0, Result: resultMessage("c1", "x", false)})
		}},
		{name: "skipped_index", build: func(f *replayFixture) {
			f.started()
			f.declared(0, "c1", "c2")
			f.add(store.KindRoundResult, recordID(f.runID, "-r0-1"),
				roundResultPayload{RunID: f.runID, Round: 0, Index: 1, Result: resultMessage("c2", "x", false)})
		}},
		{name: "duplicate_index", build: func(f *replayFixture) {
			base(f)
			f.add(store.KindRoundResult, recordID(f.runID, "-r0-0-dup"),
				roundResultPayload{RunID: f.runID, Round: 0, Index: 0, Result: resultMessage("c1", "again", false)})
		}},
		{name: "out_of_range_index", build: func(f *replayFixture) {
			f.started()
			f.declared(0, "c1")
			f.result(0, 0, "c1", "real", false)
			f.add(store.KindRoundResult, recordID(f.runID, "-r0-1"),
				roundResultPayload{RunID: f.runID, Round: 0, Index: 1, Result: resultMessage("c1", "x", false)})
		}},
		{name: "mismatched_call_id", build: func(f *replayFixture) {
			f.started()
			f.declared(0, "c1", "c2")
			f.add(store.KindRoundResult, recordID(f.runID, "-r0-0"),
				roundResultPayload{RunID: f.runID, Round: 0, Index: 0, Result: resultMessage("other", "x", false)})
		}},
		{name: "after_commit", build: func(f *replayFixture) {
			f.started()
			f.declared(0, "c1", "c2")
			f.committed(0, unknownBatch("c1", "c2"))
			f.result(0, 0, "c1", "late", false)
		}},
		{name: "batch_changes_saved_text", build: func(f *replayFixture) {
			base(f)
			f.committed(0, []llm.Message{resultMessage("c1", "altered", false), unknownBatch("c2")[0]})
		}},
		{name: "batch_changes_saved_error", build: func(f *replayFixture) {
			base(f)
			f.committed(0, []llm.Message{resultMessage("c1", "real", true), unknownBatch("c2")[0]})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReplayFixture()
			tc.build(f)
			_, err := replayAgent(f.recs).replayThread(context.Background(), "t")
			if !errors.Is(err, ErrIncompatibleLog) {
				t.Fatalf("replay err = %v, want ErrIncompatibleLog", err)
			}
		})
	}
}

func TestReplayRoundResultFinitePrefixIsImmutable(t *testing.T) {
	f := newReplayFixture()
	f.started()
	f.declared(0, "c1", "c2")
	f.result(0, 0, "c1", "real", false)
	v, err := replayAgent(f.recs).replayThread(context.Background(), "t")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if v.open == nil || len(v.open.results) != 1 {
		t.Fatalf("open = %+v", v.open)
	}
	beforeHistory := len(v.history)
	rec, err := encodeRecord(store.KindRoundCommitted, recordID(v.runID, "-c0"),
		roundCommittedPayload{RunID: v.runID, Round: 0, Results: v.open.resultsWithUnknown()})
	if err != nil {
		t.Fatal(err)
	}
	v2, err := replayRecords(v, []store.Record{rec})
	if err != nil {
		t.Fatalf("suffix replay: %v", err)
	}
	if v2.open != nil {
		t.Fatal("suffix replay must close the round")
	}
	if len(v.open.results) != 1 || !reflect.DeepEqual(v.open.results[0], resultMessage("c1", "real", false)) {
		t.Fatalf("input prefix mutated: %+v", v.open)
	}
	if len(v.history) != beforeHistory {
		t.Fatalf("input history mutated: %d -> %d", beforeHistory, len(v.history))
	}
}

func TestLegacyLogsWithoutRoundResultUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func(f *replayFixture)
		check func(t *testing.T, v threadView)
	}{
		{name: "committed_success", build: func(f *replayFixture) {
			f.started()
			f.declared(0, "c1")
			f.committed(0, []llm.Message{resultMessage("c1", "real", false)})
		}, check: func(t *testing.T, v threadView) {
			if v.open != nil {
				t.Fatalf("open = %v, want the round closed by the commit", v.open != nil)
			}
			if !v.runActive {
				t.Fatal("a committed round leaves the run active awaiting the next round or done")
			}
			if blocks := historyResultBlocks(v.history); len(blocks) != 1 || blocks[0].Content != "real" {
				t.Fatalf("history = %#v", blocks)
			}
		}},
		{name: "open_all_unknown", build: func(f *replayFixture) {
			f.started()
			f.declared(0, "c1", "c2")
		}, check: func(t *testing.T, v threadView) {
			if v.open == nil || len(v.open.results) != 0 {
				t.Fatalf("open = %+v, want a bare declaration", v.open)
			}
		}},
		{name: "cancelled_all_unknown_v2", build: func(f *replayFixture) {
			f.started()
			f.declared(0, "c1", "c2")
			fv, err := replayRecords(threadView{}, f.recs)
			if err != nil {
				t.Fatal(err)
			}
			rec, err := fv.cancelRecord()
			if err != nil {
				t.Fatal(err)
			}
			f.recs = append(f.recs, rec)
		}, check: func(t *testing.T, v threadView) {
			if v.runActive || !v.cancelled {
				t.Fatalf("active=%v cancelled=%v, want a cancelled terminal", v.runActive, v.cancelled)
			}
			if blocks := historyResultBlocks(v.history); len(blocks) != 2 {
				t.Fatalf("history = %#v", blocks)
			}
		}},
		{name: "legacy_error_open", build: func(f *replayFixture) {
			f.started()
			f.declared(0, "c1", "c2")
			f.add(store.KindError, recordID(f.runID, "-err"), map[string]any{"message": "boom"})
		}, check: func(t *testing.T, v threadView) {
			if v.runActive || v.open != nil {
				t.Fatalf("active=%v open=%v, want the legacy terminal", v.runActive, v.open != nil)
			}
			if blocks := historyResultBlocks(v.history); len(blocks) != 0 {
				t.Fatalf("legacy KindError must not synthesize history: %#v", blocks)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReplayFixture()
			tc.build(f)
			v, err := replayAgent(f.recs).replayThread(context.Background(), "t")
			if err != nil {
				t.Fatalf("replay: %v", err)
			}
			tc.check(t, v)
		})
	}
}

func TestReplayRoundResultEmptyImagesRoundTrip(t *testing.T) {
	f := newReplayFixture()
	f.started()
	f.declared(0, "c1")
	// A writer emitting "images":[] (non-nil empty) must still round-trip:
	// the closing commit carries the same block with the omitted key.
	f.recs = append(f.recs, store.Record{Kind: store.KindRoundResult, Schema: store.SchemaV1,
		ID:      recordID(f.runID, "-r0-0"),
		Payload: json.RawMessage(`{"run_id":"run","round":0,"index":0,"result":{"role":"tool","content":[{"type":"tool_result","tool_use_id":"c1","content":"ok","images":[]}]}}`)})
	f.committed(0, []llm.Message{resultMessage("c1", "ok", false)})
	f.declared(1, "c1")
	f.recs = append(f.recs, store.Record{Kind: store.KindRoundResult, Schema: store.SchemaV1,
		ID:      recordID(f.runID, "-r1-0"),
		Payload: json.RawMessage(`{"run_id":"run","round":1,"index":0,"result":{"role":"tool","content":[{"type":"tool_result","tool_use_id":"c1","content":"ok","images":[]}]}}`)})
	f.committed(1, []llm.Message{resultMessage("c1", "ok", false)})
	v, err := replayAgent(f.recs).replayThread(context.Background(), "t")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if v.open != nil {
		t.Fatal("both rounds must be closed by their commits")
	}
	if blocks := historyResultBlocks(v.history); len(blocks) != 2 || blocks[0].Content != "ok" {
		t.Fatalf("history = %#v", blocks)
	}
}

func TestKindErrorWithRoundResultKeepsPrefix(t *testing.T) {
	f := newReplayFixture()
	f.started()
	f.declared(0, "c1", "c2")
	f.result(0, 0, "c1", "real", false)
	f.add(store.KindError, recordID(f.runID, "-err"), map[string]any{"message": "boom"})
	v, err := replayAgent(f.recs).replayThread(context.Background(), "t")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if v.runActive || v.open != nil {
		t.Fatalf("active=%v open=%v, want terminal", v.runActive, v.open != nil)
	}
	blocks := historyResultBlocks(v.history)
	if len(blocks) != 2 || blocks[0].IsError || blocks[0].Content != "real" ||
		!blocks[1].IsError || !strings.Contains(blocks[1].Content, "outcome unknown") {
		t.Fatalf("history = %#v, want recorded c1 and unknown c2", blocks)
	}
}
