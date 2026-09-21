package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/dailz1/go-agent/store"
)

func TestSettleInterruptedCredential(t *testing.T) {
	st := store.NewMemory()
	want := errors.New("provider interrupted")
	ag := persistTestAgent(t, st, ErrResponse(want))
	_, err := ag.RunThread(context.Background(), "thread", "old input")
	var interrupted *RunInterruptedError
	if !errors.As(err, &interrupted) || !errors.Is(err, want) {
		t.Fatalf("RunThread = %v", err)
	}
	credential := interrupted.Settlement
	if credential == nil || credential.ThreadID != "thread" || credential.RunID == "" || credential.ExpectedHead != 1 {
		t.Fatal("interrupted run did not deliver its original settlement credential")
	}
}
