package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

// settleForTest settles over the same store instance with a fresh agent:
// ownership is keyed by (Store, ThreadID), never by Agent identity.
func settleForTest(t *testing.T, st store.Store, token SettlementToken) error {
	t.Helper()
	a := New(NewMockStreamingProvider(nil), tool.NewRegistry(), WithStore(st), WithLogger(discardLogger()))
	return a.SettleThread(context.Background(), token)
}

func runToInterrupt(t *testing.T, st store.Store, breakAt int, softFirst bool) SettlementToken {
	t.Helper()
	reg := tool.NewRegistry()
	var executions atomic.Int32
	for _, name := range []string{"one", "two"} {
		name := name
		first := name == "one"
		reg.MustRegister(mutatorTool(name, func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
			executions.Add(1)
			if first && softFirst {
				return &tool.ToolResult{Content: "soft", Status: tool.ResultError}, nil
			}
			return tool.NewTextResult(name), nil
		}))
	}
	exit := make(chan SettlementToken, 1)
	agent := New(parallelProvider(parallelToolCalls("one", "two")), reg,
		WithStore(st), WithLogger(discardLogger()),
		WithRunExitFn(func(tk SettlementToken) { exit <- tk }))
	seq, err := agent.RunStream(context.Background(), "go", WithThread("t"))
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	delivered := 0
	calls := 0
loop:
	for event, streamErr := range seq {
		if streamErr != nil {
			break
		}
		switch ev := event.(type) {
		case ToolCallEvent:
			calls++
			if breakAt == 0 && calls == 1 {
				break loop
			}
		case ToolResultEvent:
			_ = ev
			delivered++
			if delivered == breakAt {
				break loop
			}
		}
	}
	select {
	case token := <-exit:
		return token
	default:
		t.Fatalf("no settlement token after break (delivered=%d)", delivered)
		return SettlementToken{}
	}
}

type wantResult struct {
	unknown bool
	content string
	isError bool
}

func TestSettleThreadPreservesRoundResults(t *testing.T) {
	for _, tc := range []struct {
		name       string
		breakAt    int
		softFirst  bool
		wantFirst  wantResult
		wantSecond wantResult
	}{
		{name: "no_prefix", breakAt: 0, wantFirst: wantResult{unknown: true}, wantSecond: wantResult{unknown: true}},
		{name: "one_success", breakAt: 1, wantFirst: wantResult{content: "one"}, wantSecond: wantResult{unknown: true}},
		{name: "one_soft_error", breakAt: 1, softFirst: true, wantFirst: wantResult{content: "soft", isError: true}, wantSecond: wantResult{unknown: true}},
		{name: "all_recorded_open", breakAt: 2, wantFirst: wantResult{content: "one"}, wantSecond: wantResult{content: "two"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := store.NewMemory()
			token := runToInterrupt(t, st, tc.breakAt, tc.softFirst)
			if err := settleForTest(t, st, token); err != nil {
				t.Fatalf("SettleThread: %v", err)
			}
			var cancel runCancelledPayload
			for _, r := range logRecords(t, st, "t") {
				if r.Kind == store.KindRunCancelled {
					var err error
					cancel, err = decodeRunCancelled(r)
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			if cancel.RunID == "" || cancel.OpenRound == nil {
				t.Fatalf("cancel record = %+v, want an open round", cancel)
			}
			if len(cancel.Results) != 2 {
				t.Fatalf("results = %d, want one per declared call", len(cancel.Results))
			}
			first := resultBlocks(cancel.Results[0])[0]
			second := resultBlocks(cancel.Results[1])[0]
			if first.ToolUseID != "c1" || second.ToolUseID != "c2" {
				t.Fatalf("pairing = %s/%s", first.ToolUseID, second.ToolUseID)
			}
			checkWant(t, "first", first, tc.wantFirst)
			checkWant(t, "second", second, tc.wantSecond)
		})
	}
}

func checkWant(t *testing.T, label string, got llm.ToolResultBlock, want wantResult) {
	t.Helper()
	if want.unknown {
		if !got.IsError || !strings.Contains(got.Content, "outcome unknown") {
			t.Fatalf("%s = %#v, want unknown", label, got)
		}
		return
	}
	if got.IsError != want.isError || got.Content != want.content {
		t.Fatalf("%s = %#v, want content %q isError %v", label, got, want.content, want.isError)
	}
}

func TestSettleRejectsChangedRoundResults(t *testing.T) {
	base := func() (*replayFixture, *threadView) {
		f := newReplayFixture()
		f.started()
		f.declared(0, "c1", "c2")
		f.result(0, 0, "c1", "real", false)
		v, err := replayRecords(threadView{}, f.recs)
		if err != nil {
			t.Fatal(err)
		}
		return f, &v
	}
	cancelWith := func(f *replayFixture, results []llm.Message, round int) store.Record {
		roundCopy := round
		rec, err := encodeRecord(store.KindRunCancelled, recordID(f.runID, "-cancel"),
			runCancelledPayload{RunID: f.runID, OpenRound: &roundCopy, Results: results})
		if err != nil {
			t.Fatal(err)
		}
		rec.Schema = store.SchemaV2
		return rec
	}
	for _, tc := range []struct {
		name    string
		results func() []llm.Message
		round   int
	}{
		{name: "known_to_unknown", round: 0, results: func() []llm.Message {
			return unknownResults(toolCallMessage("c1", "c2"))
		}},
		{name: "known_content_changed", round: 0, results: func() []llm.Message {
			return []llm.Message{resultMessage("c1", "altered", false), resultMessage("c2", "x", true)}
		}},
		{name: "unknown_to_success", round: 0, results: func() []llm.Message {
			return []llm.Message{resultMessage("c1", "real", false), resultMessage("c2", "fabricated", false)}
		}},
		{name: "wrong_pairing", round: 0, results: func() []llm.Message {
			return []llm.Message{resultMessage("c2", "x", true), resultMessage("c1", "real", false)}
		}},
		{name: "wrong_open_round", round: 5, results: func() []llm.Message {
			return unknownResults(toolCallMessage("c1", "c2"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, v := base()
			rec := cancelWith(f, tc.results(), tc.round)
			if err := v.applyCancellation(rec); !errors.Is(err, ErrIncompatibleLog) {
				t.Fatalf("applyCancellation = %v, want ErrIncompatibleLog", err)
			}
		})
	}
	t.Run("legacy_all_unknown_without_records", func(t *testing.T) {
		f := newReplayFixture()
		f.started()
		f.declared(0, "c1", "c2")
		v, err := replayRecords(threadView{}, f.recs)
		if err != nil {
			t.Fatal(err)
		}
		rec, err := v.cancelRecord()
		if err != nil {
			t.Fatal(err)
		}
		if err := v.applyCancellation(rec); err != nil {
			t.Fatalf("legacy all-unknown cancellation = %v, want accepted", err)
		}
	})
}

func TestSettleRoundResultTokenRetry(t *testing.T) {
	t.Run("ack_lost_then_same_token", func(t *testing.T) {
		st := store.NewMemory()
		token := runToInterrupt(t, st, 1, false)
		if err := settleForTest(t, st, token); err != nil {
			t.Fatalf("first settle: %v", err)
		}
		before := len(logRecords(t, st, "t"))
		// A same-token retry after a landed cancellation is a read-only
		// confirmation: nil, and nothing is appended.
		if err := settleForTest(t, st, token); err != nil {
			t.Fatalf("retry after settle = %v, want read-only confirmation", err)
		}
		if len(logRecords(t, st, "t")) != before {
			t.Fatal("the retried settle appended a second cancel record")
		}
	})
	t.Run("late_after_successor_run", func(t *testing.T) {
		st := store.NewMemory()
		token := runToInterrupt(t, st, 1, false)
		if err := settleForTest(t, st, token); err != nil {
			t.Fatalf("first settle: %v", err)
		}
		reg := tool.NewRegistry()
		reg.MustRegister(mutatorTool("one", func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
			return tool.NewTextResult("one"), nil
		}))
		next := New(NewMockStreamingProvider([][]llm.Chunk{textChunks("fresh")}), reg,
			WithStore(st), WithLogger(discardLogger()))
		if _, err := next.Run(context.Background(), "next", WithThread("t")); err != nil {
			t.Fatalf("successor run: %v", err)
		}
		before := len(logRecords(t, st, "t"))
		if err := settleForTest(t, st, token); err != nil {
			t.Fatalf("late retry = %v, want read-only confirmation", err)
		}
		if len(logRecords(t, st, "t")) != before {
			t.Fatal("the late retry touched the successor run")
		}
	})
}
