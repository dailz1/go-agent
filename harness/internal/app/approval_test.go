package app

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/dailz1/go-agent/harness/internal/tools"
	"github.com/dailz1/go-agent/harness/internal/workspace"
	"github.com/dailz1/go-agent/tool"
)

func approvalFixture(t *testing.T, noApproval bool) (*Approvals, *RunApproval) {
	t.Helper()
	w, err := workspace.Open(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	manager := NewApprovals(noApproval)
	run, err := manager.Begin(t.Context(), "thread", w)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(run.Cancel)
	return manager, run
}

func receiveApproval(t *testing.T, manager *Approvals) ApprovalRequest {
	t.Helper()
	select {
	case request := <-manager.Requests():
		return request
	case <-time.After(5 * time.Second):
		t.Fatal("approval request did not arrive")
		return ApprovalRequest{}
	}
}

func approvalResult(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("approval did not finish")
		return nil
	}
}

func TestApprovalLateReplyAfterCancel(t *testing.T) {
	manager, run := approvalFixture(t, false)
	done := make(chan error, 1)
	go func() {
		done <- run.ApproveRead(run.Context(), ".env")
	}()
	request := receiveApproval(t, manager)
	run.Cancel()
	if manager.Respond(request.Token, request.ID, true) {
		t.Fatal("late approval accepted")
	}
	if err := approvalResult(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want cancellation", err)
	}
}

func TestApprovalRejectsWrongRunAndDuplicateReply(t *testing.T) {
	manager, run := approvalFixture(t, false)
	done := make(chan error, 1)
	go func() {
		done <- run.ApproveRead(run.Context(), ".env")
	}()
	request := receiveApproval(t, manager)
	if manager.Respond("other-run", request.ID, true) {
		t.Fatal("wrong run approved")
	}
	if !manager.Respond(request.Token, request.ID, true) {
		t.Fatal("valid approval rejected")
	}
	if manager.Respond(request.Token, request.ID, true) {
		t.Fatal("duplicate reply accepted")
	}
	if err := approvalResult(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestApprovalGrantBoundaries(t *testing.T) {
	for _, tt := range []struct {
		name string
		path string
	}{
		{name: "sensitive", path: ".env"},
		{name: "instructions", path: "AGENTS.md"},
		{name: "traversal", path: "../outside"},
		{name: "git", path: ".git/config"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			manager, run := approvalFixture(t, false)
			if err := manager.Grant(run.Token(), []string{tt.path}); err == nil {
				t.Fatal("unsafe grant accepted")
			}
		})
	}
}

func TestApprovalGrantQuotaAndNewPath(t *testing.T) {
	manager, run := approvalFixture(t, false)
	if err := manager.Grant(run.Token(), []string{"a"}); err != nil {
		t.Fatal(err)
	}
	for range 20 {
		if err := run.authorize(run.Context(), ApprovalRequest{Tool: "edit", Path: "a"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"a", "b"} {
		done := make(chan error, 1)
		go func() {
			done <- run.authorize(run.Context(), ApprovalRequest{Tool: "write", Path: path})
		}()
		request := receiveApproval(t, manager)
		if !manager.Respond(request.Token, request.ID, false) {
			t.Fatal("denial rejected")
		}
		if err := approvalResult(t, done); !errors.Is(err, tools.ErrDenied) {
			t.Fatalf("got %v, want denial", err)
		}
	}
}

func TestApprovalGrantExpiryAndShellExclusion(t *testing.T) {
	manager, run := approvalFixture(t, false)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	manager.now = func() time.Time { return now }
	if err := manager.Grant(run.Token(), []string{"a"}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- run.ApproveShell(run.Context(), "go test", ".", time.Minute)
	}()
	request := receiveApproval(t, manager)
	manager.Respond(request.Token, request.ID, false)
	if err := approvalResult(t, done); !errors.Is(err, tools.ErrDenied) {
		t.Fatal(err)
	}
	now = now.Add(30 * time.Minute)
	go func() {
		done <- run.authorize(run.Context(), ApprovalRequest{Tool: "write", Path: "a"})
	}()
	request = receiveApproval(t, manager)
	manager.Respond(request.Token, request.ID, false)
	if err := approvalResult(t, done); !errors.Is(err, tools.ErrDenied) {
		t.Fatal(err)
	}
}

func TestApprovalGrantDoesNotReleasePendingRequest(t *testing.T) {
	manager, run := approvalFixture(t, false)
	done := make(chan error, 1)
	go func() {
		done <- run.authorize(run.Context(), ApprovalRequest{Tool: "write", Path: "a"})
	}()
	request := receiveApproval(t, manager)
	if err := manager.Grant(run.Token(), []string{"a"}); err != nil {
		t.Fatal(err)
	}
	manager.Respond(request.Token, request.ID, false)
	if err := approvalResult(t, done); !errors.Is(err, tools.ErrDenied) {
		t.Fatalf("grant released existing request: %v", err)
	}
}

func TestApprovalNoApprovalStillRejectsCancellation(t *testing.T) {
	_, run := approvalFixture(t, true)
	if err := run.ApproveRead(run.Context(), ".env"); err != nil {
		t.Fatal(err)
	}
	run.Cancel()
	if err := run.ApproveShell(run.Context(), "touch forbidden", ".", time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want cancellation", err)
	}
}

func TestApprovalCredentialIsExactAndOneUse(t *testing.T) {
	manager, run := approvalFixture(t, false)
	raw := json.RawMessage(`{"command":"go test","cwd":".","timeout":60}`)
	approved := make(chan bool, 1)
	go func() { approved <- run.ApprovalFn(tool.ToolInfo{Name: "shell"}, raw) }()
	request := receiveApproval(t, manager)
	manager.Respond(request.Token, request.ID, true)
	select {
	case ok := <-approved:
		if !ok {
			t.Fatal("approval failed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("callback did not return")
	}
	ctx := context.WithValue(run.Context(), approvalCallKey{}, approvalCall{name: "shell", args: string(raw)})
	if err := run.ApproveShell(ctx, "go test", ".", time.Minute); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"go test", "go test; touch unauthorized"} {
		done := make(chan error, 1)
		go func() { done <- run.ApproveShell(ctx, command, ".", time.Minute) }()
		request := receiveApproval(t, manager)
		manager.Respond(request.Token, request.ID, false)
		if err := approvalResult(t, done); !errors.Is(err, tools.ErrDenied) {
			t.Fatalf("credential reused for %q: %v", command, err)
		}
	}
}

func TestApprovalSwitchAndCancellationRevokeGrant(t *testing.T) {
	for _, tt := range []struct {
		name   string
		thread string
		cancel bool
	}{
		{name: "switch", thread: "other"},
		{name: "cancel", thread: "thread", cancel: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			manager, run := approvalFixture(t, false)
			if err := manager.Grant(run.Token(), []string{"a"}); err != nil {
				t.Fatal(err)
			}
			if tt.cancel {
				run.Cancel()
			}
			run.Finish()
			next, err := manager.Begin(t.Context(), tt.thread, run.workspace)
			if err != nil {
				t.Fatal(err)
			}
			defer next.Cancel()
			done := make(chan error, 1)
			go func() {
				done <- next.authorize(next.Context(), ApprovalRequest{Tool: "write", Path: "a"})
			}()
			request := receiveApproval(t, manager)
			manager.Respond(request.Token, request.ID, false)
			if err := approvalResult(t, done); !errors.Is(err, tools.ErrDenied) {
				t.Fatal(err)
			}
		})
	}
}

func TestApprovalCloseRejectsPending(t *testing.T) {
	manager, run := approvalFixture(t, false)
	done := make(chan error, 1)
	go func() { done <- run.ApproveRead(run.Context(), ".env") }()
	request := receiveApproval(t, manager)
	manager.Close()
	if manager.Respond(request.Token, request.ID, true) {
		t.Fatal("closed UI accepted approval")
	}
	if err := approvalResult(t, done); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestRunFinishJoinsOwnedTool(t *testing.T) {
	manager, run := approvalFixture(t, false)
	done := make(chan error, 1)
	wrapped := runTool{Tool: sensitiveTestTool{run: run}, run: run}
	go func() {
		_, err := wrapped.Execute(run.Context(), json.RawMessage(`{}`))
		done <- err
	}()
	receiveApproval(t, manager)
	run.Finish()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	default:
		// Execute is joined, but its caller may not yet have sent its result.
		if err := approvalResult(t, done); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
	if _, err := wrapped.Execute(t.Context(), json.RawMessage(`{}`)); !errors.Is(err, context.Canceled) {
		t.Fatal("finished run tool remained executable", err)
	}
}

type sensitiveTestTool struct{ run *RunApproval }

func (s sensitiveTestTool) Info() tool.ToolInfo { return tool.ToolInfo{Name: "read"} }
func (s sensitiveTestTool) Execute(ctx context.Context, _ json.RawMessage) (*tool.ToolResult, error) {
	return nil, s.run.ApproveRead(ctx, ".env")
}
