package main

import (
	"encoding/json"
	"testing"
)

func TestScriptedProviderAndToolFuncVerifyTheExchange(t *testing.T) {
	result, calls, err := run()
	if err != nil {
		t.Fatal(err)
	}
	if result.ToolCalls != 1 || len(calls) != 1 {
		t.Fatalf("tool calls result=%d recorded=%d", result.ToolCalls, len(calls))
	}
	var input struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(calls[0].Args, &input); err != nil || input.Text != "hello from a deterministic tool" {
		t.Fatalf("recorded args=%s err=%v", calls[0].Args, err)
	}
}
