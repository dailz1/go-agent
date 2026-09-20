package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dailz1/go-agent/llm"
)

func TestParallelToolsStartTogetherAndResultsStayOrdered(t *testing.T) {
	firstStarted, secondStarted := make(chan struct{}), make(chan struct{})
	firstRelease, secondRelease := make(chan struct{}), make(chan struct{})
	var active, maximum atomic.Int32
	block := func(started chan<- struct{}, release <-chan struct{}) func(context.Context) {
		return func(ctx context.Context) {
			current := active.Add(1)
			for {
				previous := maximum.Load()
				if current <= previous || maximum.CompareAndSwap(previous, current) {
					break
				}
			}
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
			}
			active.Add(-1)
		}
	}
	one, two := newDemoTool("one"), newDemoTool("two")
	one.run, two.run = block(firstStarted, firstRelease), block(secondStarted, secondRelease)
	type outcome struct {
		resultText string
		resultIDs  []string
		err        error
	}
	outcomeCh := make(chan outcome, 1)
	go func() {
		result, err := run(one, two)
		text := ""
		var resultIDs []string
		if err == nil {
			text = result.Message.Content[0].(llm.TextBlock).Text
			for _, message := range result.History {
				for _, block := range message.Content {
					if block, ok := block.(llm.ToolResultBlock); ok {
						resultIDs = append(resultIDs, block.ToolUseID)
					}
				}
			}
		}
		outcomeCh <- outcome{resultText: text, resultIDs: resultIDs, err: err}
	}()
	await(t, firstStarted)
	await(t, secondStarted)
	if maximum.Load() != 2 {
		t.Fatalf("maximum active tools = %d, want 2", maximum.Load())
	}
	close(secondRelease)
	close(firstRelease)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	select {
	case outcome := <-outcomeCh:
		if outcome.err != nil {
			t.Fatal(outcome.err)
		}
		if outcome.resultText != "parallel work complete" {
			t.Fatalf("result text = %q", outcome.resultText)
		}
		if len(outcome.resultIDs) != 2 || outcome.resultIDs[0] != "first" || outcome.resultIDs[1] != "second" {
			t.Fatalf("tool result IDs = %v, want declaration order [first second]", outcome.resultIDs)
		}
	case <-ctx.Done():
		t.Fatal("parallel run did not finish")
	}
}

func await(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	select {
	case <-signal:
	case <-ctx.Done():
		t.Fatal("timed out waiting for tool start")
	}
}
