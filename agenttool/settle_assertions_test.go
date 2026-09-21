package agenttool_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/agent"
	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
)

func settleRecords(t *testing.T, s store.Store, id string) []store.Record {
	t.Helper()
	records, err := s.History(t.Context(), id, 0)
	if err != nil {
		t.Fatal(err)
	}
	return records
}

func settleObservedToken(t *testing.T, s store.Store, id string) agent.SettlementToken {
	t.Helper()
	records := settleRecords(t, s, id)
	var runID string
	for _, record := range records {
		if record.Kind == store.KindRunStarted {
			var payload struct {
				RunID string `json:"run_id"`
			}
			if err := json.Unmarshal(record.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			runID = payload.RunID
		}
	}
	if runID == "" {
		t.Fatalf("thread %q has no run identity", id)
	}
	return agent.SettlementToken{ThreadID: id, RunID: runID, ExpectedHead: int64(len(records))}
}

func settleUnchanged(t *testing.T, s store.Store, id string, before []store.Record) {
	t.Helper()
	got := settleRecords(t, s, id)
	if len(got) != len(before) {
		t.Fatalf("thread %q record count = %d, want %d", id, len(got), len(before))
	}
	for index, record := range got {
		want := before[index]
		sameIdentity := record.Seq == want.Seq && record.ID == want.ID
		sameContent := record.Kind == want.Kind && record.Schema == want.Schema && bytes.Equal(record.Payload, want.Payload)
		if !sameIdentity || !sameContent || !record.RecordedAt.Equal(want.RecordedAt) {
			t.Fatalf("thread %q record %d changed: before=%+v after=%+v", id, index, want, record)
		}
	}
}

func settleCancelled(t *testing.T, a *agent.Agent, token agent.SettlementToken, ids ...string) *agent.RunResult {
	t.Helper()
	result, err := a.ResumeThread(t.Context(), token.ThreadID)
	if err != nil || result == nil || !result.Cancelled || result.ThreadID != token.ThreadID {
		t.Fatalf("cancelled snapshot = %+v, %v", result, err)
	}
	if len(result.History) != 2+len(ids) || !reflect.DeepEqual(result.Message, llm.Message{}) {
		t.Fatalf("cancellation imported child history or fabricated an answer: %+v", result)
	}
	calls, results := []string{}, []string{}
	for _, message := range result.History {
		for _, block := range message.Content {
			switch block := block.(type) {
			case llm.ToolUseBlock:
				if message.Role != llm.RoleAssistant || len(results) != 0 {
					t.Fatalf("misplaced tool declaration: %+v", message)
				}
				calls = append(calls, block.ID)
			case llm.ToolResultBlock:
				index := len(results)
				if message.Role != llm.RoleTool || index >= len(calls) || calls[index] != block.ToolUseID {
					t.Fatalf("unpaired or reordered result: %+v", message)
				}
				if !block.IsError || !strings.Contains(block.Content, "unknown") {
					t.Fatalf("uncommitted call must remain unknown: %+v", block)
				}
				results = append(results, block.ToolUseID)
			}
		}
	}
	if !reflect.DeepEqual(calls, ids) || !reflect.DeepEqual(results, ids) {
		t.Fatalf("calls/results = %v/%v, want %v", calls, results, ids)
	}
	return result
}

func settleOneCancellation(t *testing.T, s store.Store, token agent.SettlementToken) {
	t.Helper()
	records := settleRecords(t, s, token.ThreadID)
	if int64(len(records)) != token.ExpectedHead+1 {
		t.Fatalf("settlement must append exactly one terminal record: %+v", records)
	}
	count := 0
	for _, record := range records {
		if record.Kind == store.KindRunCancelled {
			count++
			if record.Seq != token.ExpectedHead || record.ID != token.RunID+"-cancel" {
				t.Fatalf("cancel record does not bind original credential: %+v, %+v", record, token)
			}
		}
	}
	if count != 1 {
		t.Fatalf("cancel record count = %d, want 1", count)
	}
}

func settleNestedTokens(t *testing.T, err error, parent, child agent.SettlementToken) {
	t.Helper()
	var outer *agent.RunInterruptedError
	if !errors.As(err, &outer) || outer.Settlement == nil || *outer.Settlement != parent {
		t.Fatalf("outer error does not carry parent credential: %v", err)
	}
	tokens := []agent.SettlementToken{}
	for current := err; current != nil; current = errors.Unwrap(current) {
		if interrupted, ok := current.(*agent.RunInterruptedError); ok {
			if interrupted.Settlement == nil || interrupted.ThreadID != interrupted.Settlement.ThreadID {
				t.Fatalf("missing or misrouted settlement credential: %+v", interrupted)
			}
			tokens = append(tokens, *interrupted.Settlement)
		}
	}
	if !reflect.DeepEqual(tokens, []agent.SettlementToken{parent, child}) {
		t.Fatalf("nested credentials = %+v, want parent %+v then child %+v", tokens, parent, child)
	}
}
