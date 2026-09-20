package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/dailz1/go-agent/tool"
)

func TestParallelCancelPlusHardError(t *testing.T) {
	calls := parallelToolCalls("one", "two", "three")
	started, returned, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	reg := tool.NewRegistry()
	reg.MustRegister(&parallelTestTool{info: tool.ToolInfo{Name: "one"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		close(started)
		<-release
		return tool.NewTextResult("one"), nil
	}})
	reg.MustRegister(&parallelTestTool{info: tool.ToolInfo{Name: "two"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		close(returned)
		return nil, errors.New("boom")
	}})
	reg.MustRegister(&parallelTestTool{info: tool.ToolInfo{Name: "three"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult("wrong"), nil
	}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	seq, err := New(parallelProvider(calls), reg, WithToolConcurrency(2), WithLogger(discardLogger())).RunStream(ctx, "go")
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	terminal := make(chan error, 1)
	go func() {
		for _, streamErr := range seq {
			if streamErr != nil {
				terminal <- streamErr
				return
			}
		}
		terminal <- nil
	}()
	waitParallelSignal(t, started)
	waitParallelSignal(t, returned)
	cancel()
	close(release)
	guard, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	select {
	case err := <-terminal:
		if err == nil || !containsAll(err.Error(), "two", "execute: boom") {
			t.Fatalf("cancel plus hard error = %v", err)
		}
	case <-guard.Done():
		t.Fatal("cancel plus hard error did not drain")
	}
}
