package agent

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/dailz1/go-agent/pkg/store"
	"github.com/dailz1/go-agent/pkg/tool"
)

func TestParallelInfoPanicLeavesRecoverableDeclaration(t *testing.T) {
	st := store.NewMemory()
	panicTool := &plannerInfoPanicTool{}
	first := &parallelTestTool{info: tool.ToolInfo{Name: "one"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult("wrong"), nil
	}}
	third := &parallelTestTool{info: tool.ToolInfo{Name: "three"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult("wrong"), nil
	}}
	reg := tool.NewRegistry()
	reg.MustRegister(first)
	reg.MustRegister(panicTool)
	reg.MustRegister(third)
	provider := &plannerArmProvider{MockStreamingProvider: parallelProvider(parallelToolCalls("one", "panic_info", "three")), arm: func() { panicTool.armed.Store(true) }}
	agent := New(provider, reg, WithStore(st), WithToolConcurrency(2), WithLogger(discardLogger()))
	if _, err := agent.RunThread(context.Background(), "t", "go"); err == nil {
		t.Fatal("RunThread accepted Info panic")
	}
	if first.calls.Load() != 0 || panicTool.execCalls.Load() != 0 || third.calls.Load() != 0 || hasKind(t, st, "t", store.KindRoundCommitted) {
		t.Fatal("Info panic executed or committed its round")
	}
	panicTool.armed.Store(false)
	provider.arm = func() {}
	result, err := agent.ResumeThread(context.Background(), "t")
	if err != nil {
		t.Fatalf("ResumeThread: %v", err)
	}
	unknown := historyResultBlocks(result.History)
	if len(unknown) < 3 || unknown[0].ToolUseID != "c1" || unknown[1].ToolUseID != "c2" || unknown[2].ToolUseID != "c3" {
		t.Fatalf("recovered results = %#v", unknown)
	}
}

func TestParallelSamePlanRegistrationRemainsFrozen(t *testing.T) {
	reg := tool.NewRegistry()
	late := &parallelTestTool{info: tool.ToolInfo{Name: "late"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult("wrong"), nil
	}}
	register := &parallelTestTool{info: tool.ToolInfo{Name: "register"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		if err := reg.Register(late); err != nil {
			return nil, err
		}
		return tool.NewTextResult("registered"), nil
	}}
	reg.MustRegister(register)
	result, err := New(parallelProvider(parallelToolCalls("register", "late")), reg, WithToolConcurrency(2), WithLogger(discardLogger())).Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	results := historyResultBlocks(result.History)
	if late.calls.Load() != 0 || len(results) != 2 || !results[1].IsError || !contains(results[1].Content, "not found") {
		t.Fatalf("same-plan results = %#v, late calls = %d", results, late.calls.Load())
	}
}

func TestParallelWorkerPoolCapsConcurrentSuccess(t *testing.T) {
	const calls, workers = 5, 2
	reg := tool.NewRegistry()
	started := make(chan struct{}, calls)
	release := make(chan struct{})
	var mu sync.Mutex
	active, peak := 0, 0
	for index := range calls {
		name := string(rune('a' + index))
		reg.MustRegister(&parallelTestTool{info: tool.ToolInfo{Name: name}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
			mu.Lock()
			active++
			if active > peak {
				peak = active
			}
			mu.Unlock()
			started <- struct{}{}
			<-release
			mu.Lock()
			active--
			mu.Unlock()
			return tool.NewTextResult(name), nil
		}})
	}
	names := []string{"a", "b", "c", "d", "e"}
	outcome := make(chan error, 1)
	go func() {
		_, err := New(parallelProvider(parallelToolCalls(names...)), reg, WithToolConcurrency(workers), WithLogger(discardLogger())).Run(context.Background(), "go")
		outcome <- err
	}()
	waitParallelSignal(t, started)
	waitParallelSignal(t, started)
	mu.Lock()
	gotPeak := peak
	mu.Unlock()
	if gotPeak != workers {
		t.Fatalf("peak workers = %d, want %d", gotPeak, workers)
	}
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	select {
	case err := <-outcome:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("worker pool did not drain")
	}
}
