package codemode

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

type fakeHost struct {
	replies chan CallReply
	nextID  uint64
}

func (h *fakeHost) Submit(string, json.RawMessage) (uint64, *Failure) {
	h.nextID++
	return h.nextID, nil
}

func (h *fakeHost) Reserve() (CallReply, bool) {
	reply, ok := <-h.replies
	return reply, ok
}

func (h *fakeHost) FinishDelivery(uint64) {}

type nopControl struct{}

func (nopControl) RegisterInterrupt(func())       {}
func (nopControl) StopRequested() bool            { return false }
func (nopControl) ScriptSettled()                 {}
func (nopControl) PrintString(string) (int, bool) { return 0, false }

func fakeLimits() Limits {
	return Limits{
		SourceBytes: 64 << 10, ArgsBytes: 64 << 10, HostResultBytes: 1 << 20,
		HostDataBytes: 8 << 20, MaxImages: 8, StdoutBytes: 64 << 10,
		OutputBytes: 1 << 20, ProjectionDepth: 32, ProjectionElements: 100000,
	}
}

func TestGojaLayerAwait(t *testing.T) {
	exec := NewGojaExecutor()
	host := &fakeHost{replies: make(chan CallReply, 8)}
	req := RunRequest{
		Source:     `const r = await tools["t"]({}); return r.value + 1;`,
		Generation: "g1",
		Tools:      []ToolDescriptor{{Name: "t"}},
		Host:       host,
		Limits:     fakeLimits(),
		Control:    nopControl{},
	}
	done := make(chan RunOutcome, 1)
	go func() {
		out, err := exec.Run(context.Background(), req)
		if err != nil {
			t.Errorf("run error: %v", err)
		}
		done <- out
	}()
	time.Sleep(100 * time.Millisecond)
	tv, _ := json.Marshal(ToolValue{Value: json.RawMessage("41")})
	host.replies <- CallReply{CallID: 1, Value: tv}
	select {
	case out := <-done:
		if out.Failure != nil {
			t.Fatalf("unexpected failure: %s", out.Failure.Message)
		}
		if string(out.Value) != "42" {
			t.Fatalf("want 42, got %s", out.Value)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("run did not settle")
	}
}

func TestGojaLayerSyncReturn(t *testing.T) {
	exec := NewGojaExecutor()
	host := &fakeHost{replies: make(chan CallReply, 8)}
	req := RunRequest{
		Source:     `return "sync";`,
		Generation: "g1",
		Tools:      []ToolDescriptor{{Name: "t"}},
		Host:       host,
		Limits:     fakeLimits(),
		Control:    nopControl{},
	}
	done := make(chan RunOutcome, 1)
	go func() {
		out, _ := exec.Run(context.Background(), req)
		done <- out
	}()
	select {
	case out := <-done:
		if out.Failure != nil {
			t.Fatalf("unexpected failure: %s", out.Failure.Message)
		}
		if string(out.Value) != `"sync"` {
			t.Fatalf("want \"sync\", got %s", out.Value)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("sync run did not settle")
	}
}

func TestGojaLayerSyncThrow(t *testing.T) {
	exec := NewGojaExecutor()
	host := &fakeHost{replies: make(chan CallReply, 8)}
	req := RunRequest{
		Source:     `throw new Error("early")`,
		Generation: "g1",
		Tools:      []ToolDescriptor{{Name: "t"}},
		Host:       host,
		Limits:     fakeLimits(),
		Control:    nopControl{},
	}
	done := make(chan RunOutcome, 1)
	go func() {
		out, _ := exec.Run(context.Background(), req)
		done <- out
	}()
	select {
	case out := <-done:
		if out.Failure == nil || out.Failure.Kind != FailureScript {
			t.Fatalf("want script failure, got %+v", out)
		}
		if out.Failure.Message != "Error: early" {
			t.Logf("note: message %q", out.Failure.Message)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("sync throw did not settle")
	}
}
