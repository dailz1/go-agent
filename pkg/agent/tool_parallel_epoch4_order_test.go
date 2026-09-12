package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/dailz1/go-agent/pkg/store"
	"github.com/dailz1/go-agent/pkg/tool"
)

func TestParallelC2FirstKeepsEventHistoryAndCommitOrder(t *testing.T) {
	oneStarted, twoStarted := make(chan struct{}), make(chan struct{})
	oneRelease, twoRelease := make(chan struct{}), make(chan struct{})
	twoCompleted := make(chan struct{})
	reg := tool.NewRegistry()
	reg.MustRegister(&parallelTestTool{info: tool.ToolInfo{Name: "one"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		close(oneStarted)
		<-oneRelease
		return tool.NewTextResult("one"), nil
	}})
	reg.MustRegister(&parallelTestTool{info: tool.ToolInfo{Name: "two"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		close(twoStarted)
		<-twoRelease
		close(twoCompleted)
		return tool.NewTextResult("two"), nil
	}})
	st := store.NewMemory()
	agent := New(parallelProvider(parallelToolCalls("one", "two")), reg, WithStore(st), WithToolConcurrency(2), WithLogger(discardLogger()))
	outcome := make(chan struct {
		events []AgentEvent
		err    error
	}, 1)
	go func() {
		seq, err := agent.RunThreadStream(context.Background(), "t", "go")
		var events []AgentEvent
		if err == nil {
			for event, streamErr := range seq {
				if streamErr != nil {
					err = streamErr
					break
				}
				events = append(events, event)
			}
		}
		outcome <- struct {
			events []AgentEvent
			err    error
		}{events, err}
	}()
	waitParallelSignal(t, oneStarted)
	waitParallelSignal(t, twoStarted)
	close(twoRelease)
	guard, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	select {
	case <-twoCompleted:
	case <-guard.Done():
		t.Fatal("c2 did not complete")
	}
	close(oneRelease)
	var result struct {
		events []AgentEvent
		err    error
	}
	select {
	case result = <-outcome:
	case <-guard.Done():
		t.Fatal("parallel outcome did not finish")
	}
	if result.err != nil {
		t.Fatal(result.err)
	}
	var pairs []string
	for _, event := range result.events {
		if value, ok := event.(ToolResultEvent); ok {
			pairs = append(pairs, value.ID+":"+value.Result.Content)
		}
	}
	if len(pairs) != 2 || pairs[0] != "c1:one" || pairs[1] != "c2:two" {
		t.Fatalf("event pairs = %v", pairs)
	}
	for _, event := range result.events {
		if done, ok := event.(DoneEvent); ok {
			history := historyResultBlocks(done.History)
			if len(history) != 2 || history[0].ToolUseID != "c1" || history[0].Content != "one" || history[1].ToolUseID != "c2" || history[1].Content != "two" {
				t.Fatalf("Done history = %#v", history)
			}
		}
	}
	var committed roundCommittedPayload
	for _, record := range logRecords(t, st, "t") {
		if record.Kind == store.KindRoundCommitted {
			if err := strictDecode(record.Payload, &committed); err != nil {
				t.Fatal(err)
			}
		}
	}
	first, second := resultBlocks(committed.Results[0])[0], resultBlocks(committed.Results[1])[0]
	if first.ToolUseID+":"+first.Content+"/"+second.ToolUseID+":"+second.Content != "c1:one/c2:two" {
		t.Fatalf("commit = %#v", committed.Results)
	}
}
