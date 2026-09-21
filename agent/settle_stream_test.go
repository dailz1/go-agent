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

func TestSettleStreamExit(t *testing.T) {
	for _, test := range []struct {
		name      string
		stop      string
		anonymous bool
		callback  bool
		maxIter   int
	}{
		{"E31_first_call", "call", false, true, 10},
		{"E32_first_result", "result", false, true, 10},
		{"E33_text", "text", false, true, 10},
		{"E34_anonymous", "call", true, true, 10},
		{"E34_no_callback", "call", true, false, 10},
		{"E35_done", "done", false, true, 10},
		{"E36_maxiter_call", "call", false, true, 1},
		{"E36_maxiter_result", "result", false, true, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			st := store.NewMemory()
			chunks := []llm.Chunk{
				llm.ToolCallStartChunk{Index: 0, ID: "c1", Name: "echo"}, llm.ToolCallArgsChunk{Index: 0, Delta: `{}`},
				llm.ToolCallStartChunk{Index: 1, ID: "c2", Name: "echo"}, llm.ToolCallArgsChunk{Index: 1, Delta: `{}`},
				llm.DoneChunk{FinishReason: "tool_calls"},
			}
			if test.stop == "text" || test.stop == "done" {
				chunks = []llm.Chunk{llm.TextDeltaChunk{Text: "partial"}, llm.DoneChunk{FinishReason: "stop"}}
			}
			p := NewMockStreamingProvider([][]llm.Chunk{chunks, {llm.TextDeltaChunk{Text: "redirected"}, llm.DoneChunk{FinishReason: "stop"}}})
			reg := tool.NewRegistry()
			calls := 0
			reg.MustRegister(&settleCountingTool{calls: &calls})
			ag := New(p, reg, WithStore(st), WithMaxIter(test.maxIter), WithLogger(discardLogger()))
			var target SettlementToken
			callbacks := 0
			if test.callback {
				WithRunExitFn(func(value SettlementToken) {
					callbacks++
					target = value
					// E51: callback precedes release and cannot reenter this thread.
					if _, err := ag.SettlementTarget(context.Background(), value.ThreadID); !errors.Is(err, ErrThreadBusy) {
						t.Errorf("callback ownership = %v", err)
					}
				})(ag)
			}
			seq, err := ag.RunThreadStream(context.Background(), "t", "old")
			if test.anonymous {
				seq, err = ag.RunStream(context.Background(), "old")
			}
			if err != nil {
				t.Fatal(err)
			}
			for event, err := range seq {
				if err != nil {
					t.Fatal(err)
				}
				kind := ""
				switch event.(type) {
				case TextDeltaEvent:
					kind = "text"
				case ToolCallEvent:
					kind = "call"
				case ToolResultEvent:
					kind = "result"
				case DoneEvent:
					kind = "done"
				}
				if kind == test.stop {
					break
				}
			}
			if !test.callback {
				if callbacks != 0 || target.ThreadID != "" {
					t.Fatal("unexpected anonymous identity delivery")
				}
				return
			}
			if callbacks != 1 || target.ThreadID == "" || target.RunID == "" {
				t.Fatalf("exit target = %#v callbacks=%d", target, callbacks)
			}
			beforeCalls, beforeProvider := calls, p.index
			terminal := test.maxIter == 1 || test.stop == "done"
			err = ag.SettleThread(context.Background(), target)
			if terminal {
				if !errors.Is(err, ErrNothingToSettle) {
					t.Fatalf("completed run settled: %v", err)
				}
				if _, err := ag.ResumeThread(context.Background(), target.ThreadID); !errors.Is(err, ErrNothingToResume) {
					t.Fatal(err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			res, err := ag.ResumeThread(context.Background(), target.ThreadID)
			if err != nil || !res.Cancelled || calls != beforeCalls || p.index != beforeProvider || callbacks != 1 {
				t.Fatalf("settle/snapshot executed: %#v %v", res, err)
			}
			wantLen := 4
			if test.stop == "text" {
				wantLen = 1
			}
			if len(res.History) != wantLen {
				t.Fatalf("partial data persisted: %#v", res.History)
			}
			if wantLen == 4 {
				for i, id := range []string{"c1", "c2"} {
					results := toolResultBlocks(res.History[2+i])
					if len(results) != 1 || results[0].ToolUseID != id || !results[0].IsError || !strings.Contains(results[0].Content, "unknown") {
						t.Fatalf("result %d = %#v", i, results)
					}
				}
			}
			if test.stop == "call" && calls != 0 {
				t.Fatal("tool executed after TC break")
			}
			// Exercise the public streaming redirect surface, not just RunThread.
			next, err := ag.RunThreadStream(context.Background(), target.ThreadID, "new exact input")
			if err != nil {
				t.Fatal(err)
			}
			var done DoneEvent
			for event, err := range next {
				if err != nil {
					t.Fatal(err)
				}
				if d, ok := event.(DoneEvent); ok {
					done = d
				}
			}
			if calls != beforeCalls || messageString(p.LastMessages[len(p.LastMessages)-1]) != "new exact input" {
				t.Fatal("redirect reexecuted tools or lost input")
			}
			if _, err := partitionHistory(done.History); err != nil {
				t.Fatal(err)
			}
			v, err := ag.replayThread(context.Background(), target.ThreadID)
			if err != nil || !reflect.DeepEqual(done.History, seededHistory(v.system, v.history)) {
				t.Fatalf("stream/replay mismatch: %v", err)
			}
		})
	}
}

type settleCountingTool struct{ calls *int }

func (*settleCountingTool) Info() tool.ToolInfo { return tool.ToolInfo{Name: "echo"} }
func (s *settleCountingTool) Execute(context.Context, json.RawMessage) (*tool.ToolResult, error) {
	*s.calls++
	return tool.NewTextResult("executed"), nil
}
