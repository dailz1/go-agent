package main

import "testing"

func TestAgentToolDelegatesThroughTheParentRegistry(t *testing.T) {
	result, err := run()
	if err != nil {
		t.Fatal(err)
	}
	if result.ToolCalls != 1 || result.Truncated {
		t.Fatalf("unexpected result: %+v", result)
	}
}
