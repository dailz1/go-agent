package main

import "testing"

func TestRecoverThreadUsesResumeThread(t *testing.T) {
	result, err := recoverThread()
	if err != nil {
		t.Fatal(err)
	}
	if result.ThreadID != threadID || result.Truncated || result.ToolCalls != 0 {
		t.Fatalf("unexpected resumed result: %+v", result)
	}
}
