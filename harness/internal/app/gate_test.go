package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dailz1/go-agent/harness/internal/snapshot"
	"github.com/dailz1/go-agent/harness/internal/tools"
	"github.com/dailz1/go-agent/tool"
)

type sessionStores struct {
	snapshots *snapshot.Store
	outputs   *tools.OutputStore
}

func openStores(t *testing.T, s *Startup) *sessionStores {
	t.Helper()
	dir := t.TempDir()
	snapshots, err := snapshot.Open(filepath.Join(dir, "snapshots"), s.Workspace, "thread", 0)
	if err != nil {
		t.Fatal(err)
	}
	outputs, err := tools.OpenOutputStore(filepath.Join(dir, "outputs"))
	if err != nil {
		snapshots.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		snapshots.Close()
		outputs.Close()
	})
	return &sessionStores{snapshots: snapshots, outputs: outputs}
}

type gateFixture struct {
	manager   *Approvals
	run       *RunApproval
	registry  *tool.Registry
	stores    *sessionStores
	workspace string
}

func beginGateFixture(t *testing.T, noApproval bool) *gateFixture {
	t.Helper()
	runtime := prepareStartup(t, noApproval)
	stores := openStores(t, runtime)
	run, registry, err := runtime.BeginRun(t.Context(), "thread", stores.snapshots, stores.outputs)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(run.Cancel)
	return &gateFixture{
		manager: runtime.Approvals, run: run, registry: registry,
		stores: stores, workspace: runtime.Workspace.Path(),
	}
}

func diskPut(t *testing.T, f *gateFixture, path, text string) {
	t.Helper()
	full := filepath.Join(f.workspace, path)
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(text), 0640); err != nil {
		t.Fatal(err)
	}
}

func diskGet(t *testing.T, f *gateFixture, path string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.workspace, path))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func callRunTool(t *testing.T, f *gateFixture, name string, args any) (*tool.ToolResult, error) {
	t.Helper()
	impl, ok := f.registry.Get(name)
	if !ok {
		t.Fatalf("missing tool %s", name)
	}
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return impl.Execute(f.run.Context(), raw)
}

func mustCallRunTool(t *testing.T, f *gateFixture, name string, args any) *tool.ToolResult {
	t.Helper()
	result, err := callRunTool(t, f, name, args)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError() {
		t.Fatalf("%s: %s", name, result.Content)
	}
	return result
}

func readHash(t *testing.T, f *gateFixture, path string) string {
	t.Helper()
	var got struct {
		Hash string `json:"hash"`
	}
	if err := json.Unmarshal([]byte(mustCallRunTool(t, f, "read", map[string]any{"path": path}).Content), &got); err != nil {
		t.Fatal(err)
	}
	return got.Hash
}

func noPendingRequest(t *testing.T, manager *Approvals) {
	t.Helper()
	select {
	case request := <-manager.Requests():
		t.Fatalf("unexpected approval request: %+v", request)
	default:
	}
}

func TestGateEditApprovesRecordsAndRestores(t *testing.T) {
	f := beginGateFixture(t, false)
	diskPut(t, f, "a", "user dirty\n")
	hash := readHash(t, f, "a")
	type finished struct {
		result *tool.ToolResult
		err    error
	}
	done := make(chan finished, 1)
	go func() {
		result, err := callRunTool(t, f, "edit", map[string]any{
			"path": "a", "expected_hash": hash, "old_text": "dirty", "new_text": "clean",
		})
		done <- finished{result, err}
	}()
	request := receiveApproval(t, f.manager)
	if request.Token != f.run.Token() || request.Tool != "edit" || request.Path != "a" ||
		request.Change.Before != "user dirty\n" || request.Change.After != "user clean\n" ||
		request.Arguments == "" || request.ThreadID != "thread" || request.Workspace != f.workspace {
		t.Fatalf("approval request = %+v", request)
	}
	if !f.manager.Respond(request.Token, request.ID, true) {
		t.Fatal("valid approval rejected")
	}
	got := <-done
	if got.err != nil || got.result.IsError() {
		t.Fatalf("edit = %v, %v", got.result, got.err)
	}
	if diskGet(t, f, "a") != "user clean\n" {
		t.Fatalf("disk = %q", diskGet(t, f, "a"))
	}
	records, err := f.stores.snapshots.List(t.Context())
	if err != nil || len(records) != 1 || records[0].State != snapshot.Applied ||
		records[0].Change.Before != "user dirty\n" || records[0].Change.After != "user clean\n" ||
		records[0].Credential.Generation == 0 || records[0].Credential.ToolSequence != 1 {
		t.Fatalf("records = %+v, %v", records, err)
	}
	if _, err := f.manager.Restore(t.Context(), f.stores.snapshots, records[0].ID); err == nil {
		t.Fatal("restore allowed during an active run")
	}
	f.run.Finish()
	if _, err := f.manager.Restore(t.Context(), f.stores.snapshots, records[0].ID); err != nil {
		t.Fatal(err)
	}
	if diskGet(t, f, "a") != "user dirty\n" {
		t.Fatalf("restored disk = %q", diskGet(t, f, "a"))
	}
}

func TestGateDenialLeavesZeroSideEffects(t *testing.T) {
	f := beginGateFixture(t, false)
	diskPut(t, f, "a", "keep")
	hash := readHash(t, f, "a")
	type finished struct {
		result *tool.ToolResult
		err    error
	}
	done := make(chan finished, 1)
	go func() {
		result, err := callRunTool(t, f, "edit", map[string]any{
			"path": "a", "expected_hash": hash, "old_text": "keep", "new_text": "gone",
		})
		done <- finished{result, err}
	}()
	request := receiveApproval(t, f.manager)
	if !f.manager.Respond(request.Token, request.ID, false) {
		t.Fatal("denial rejected")
	}
	got := <-done
	if got.err != nil || !got.result.IsError() {
		t.Fatalf("denial = %v, %v", got.result, got.err)
	}
	if diskGet(t, f, "a") != "keep" {
		t.Fatal("denied edit changed disk")
	}
	records, err := f.stores.snapshots.List(t.Context())
	if err != nil || len(records) != 0 {
		t.Fatalf("denied edit recorded snapshots: %+v, %v", records, err)
	}
}

func TestGateLateApprovalAfterCancelExecutesNothing(t *testing.T) {
	f := beginGateFixture(t, false)
	diskPut(t, f, "a", "keep")
	hash := readHash(t, f, "a")
	done := make(chan error, 1)
	go func() {
		_, err := callRunTool(t, f, "edit", map[string]any{
			"path": "a", "expected_hash": hash, "old_text": "keep", "new_text": "gone",
		})
		done <- err
	}()
	request := receiveApproval(t, f.manager)
	f.run.Cancel()
	if f.manager.Respond(request.Token, request.ID, true) {
		t.Fatal("late approval accepted after cancel")
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("late approval executed or misreported: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("edit did not return after cancel")
	}
	if diskGet(t, f, "a") != "keep" {
		t.Fatal("cancelled run changed disk")
	}
	records, err := f.stores.snapshots.List(t.Context())
	if err != nil || len(records) != 0 {
		t.Fatalf("cancelled run recorded snapshots: %+v, %v", records, err)
	}
}

func TestGateNoApprovalStillSnapshots(t *testing.T) {
	f := beginGateFixture(t, true)
	diskPut(t, f, "a", "auto")
	hash := readHash(t, f, "a")
	mustCallRunTool(t, f, "edit", map[string]any{
		"path": "a", "expected_hash": hash, "old_text": "auto", "new_text": "done",
	})
	noPendingRequest(t, f.manager)
	if diskGet(t, f, "a") != "done" {
		t.Fatal("no-approval edit did not apply")
	}
	records, err := f.stores.snapshots.List(t.Context())
	if err != nil || len(records) != 1 || records[0].State != snapshot.Applied {
		t.Fatalf("records = %+v, %v", records, err)
	}
}

func TestGateGrantCoversExactSetOnly(t *testing.T) {
	f := beginGateFixture(t, false)
	diskPut(t, f, "a", "one")
	diskPut(t, f, "b", "two")
	readHash(t, f, "a")
	readHash(t, f, "b")
	if err := f.manager.Grant(f.run.Token(), []string{"a"}); err != nil {
		t.Fatal(err)
	}
	mustCallRunTool(t, f, "write", map[string]any{"path": "a", "expected_hash": readHash(t, f, "a"), "text": "granted"})
	noPendingRequest(t, f.manager)
	if diskGet(t, f, "a") != "granted" {
		t.Fatal("granted write did not apply")
	}
	for _, tt := range []struct {
		tool string
		args map[string]any
	}{
		{tool: "write", args: map[string]any{"path": "b", "expected_hash": readHash(t, f, "b"), "text": "asked"}},
		{tool: "shell", args: map[string]any{"command": "true"}},
	} {
		done := make(chan error, 1)
		go func() {
			_, err := callRunTool(t, f, tt.tool, tt.args)
			done <- err
		}()
		request := receiveApproval(t, f.manager)
		f.manager.Respond(request.Token, request.ID, false)
		if err := approvalResult(t, done); err != nil {
			t.Fatalf("%s outside grant did not stay gated: %v", tt.tool, err)
		}
	}
	if diskGet(t, f, "b") != "two" {
		t.Fatal("ungated write applied")
	}
}

func TestShellToolBoundToRunApproval(t *testing.T) {
	f := beginGateFixture(t, false)
	done := make(chan *tool.ToolResult, 1)
	go func() {
		result, err := callRunTool(t, f, "shell", map[string]any{"command": "printf created > made.txt", "timeout": 30})
		if err != nil {
			t.Errorf("shell: %v", err)
			return
		}
		done <- result
	}()
	request := receiveApproval(t, f.manager)
	if request.Tool != "shell" || request.Command != "printf created > made.txt" ||
		request.Cwd != "." || request.Timeout != 30*time.Second {
		t.Fatalf("shell request = %+v", request)
	}
	if !f.manager.Respond(request.Token, request.ID, true) {
		t.Fatal("approval rejected")
	}
	select {
	case result := <-done:
		if result.IsError() {
			t.Fatalf("shell failed: %s", result.Content)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("shell did not finish")
	}
	if diskGet(t, f, "made.txt") != "created" {
		t.Fatal("approved shell did not run")
	}
	f.run.Cancel()
	if _, err := callRunTool(t, f, "shell", map[string]any{"command": "true"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("shell survived cancel: %v", err)
	}
}

func TestKernelApprovalCredentialSkipsSecondPrompt(t *testing.T) {
	f := beginGateFixture(t, false)
	diskPut(t, f, "a", "once")
	hash := readHash(t, f, "a")
	raw, err := json.Marshal(map[string]any{
		"path": "a", "expected_hash": hash, "old_text": "once", "new_text": "twice",
	})
	if err != nil {
		t.Fatal(err)
	}
	approved := make(chan bool, 1)
	go func() {
		approved <- f.run.ApprovalFn(tool.ToolInfo{Name: "edit"}, raw)
	}()
	request := receiveApproval(t, f.manager)
	if request.Tool != "edit" || request.Arguments != string(raw) {
		t.Fatalf("kernel callback request = %+v", request)
	}
	f.manager.Respond(request.Token, request.ID, true)
	if ok := <-approved; !ok {
		t.Fatal("kernel approval denied")
	}
	mustCallRunTool(t, f, "edit", json.RawMessage(raw))
	noPendingRequest(t, f.manager)
	if diskGet(t, f, "a") != "twice" {
		t.Fatal("permitted edit did not apply")
	}
	// The one-use credential is gone: the same call asks again.
	hash = readHash(t, f, "a")
	done := make(chan error, 1)
	go func() {
		_, err := callRunTool(t, f, "edit", map[string]any{
			"path": "a", "expected_hash": hash, "old_text": "twice", "new_text": "thrice",
		})
		done <- err
	}()
	request = receiveApproval(t, f.manager)
	f.manager.Respond(request.Token, request.ID, false)
	if err := approvalResult(t, done); err != nil {
		t.Fatalf("spent credential reused: %v", err)
	}
}

func TestBeginRunMintsDistinctGenerations(t *testing.T) {
	runtime := prepareStartup(t, true)
	stores := openStores(t, runtime)
	diskPut(t, &gateFixture{workspace: runtime.Workspace.Path()}, "a", "v0")
	first, firstRegistry, err := runtime.BeginRun(t.Context(), "thread", stores.snapshots, stores.outputs)
	if err != nil {
		t.Fatal(err)
	}
	firstFixture := &gateFixture{manager: runtime.Approvals, run: first, registry: firstRegistry, stores: stores, workspace: runtime.Workspace.Path()}
	readHash(t, firstFixture, "a")
	mustCallRunTool(t, firstFixture, "edit", map[string]any{
		"path": "a", "expected_hash": readHash(t, firstFixture, "a"), "old_text": "v0", "new_text": "v1",
	})
	first.Cancel()
	first.Finish()
	second, secondRegistry, err := runtime.BeginRun(t.Context(), "thread", stores.snapshots, stores.outputs)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Cancel()
	secondFixture := &gateFixture{manager: runtime.Approvals, run: second, registry: secondRegistry, stores: stores, workspace: runtime.Workspace.Path()}
	readHash(t, secondFixture, "a")
	mustCallRunTool(t, secondFixture, "edit", map[string]any{
		"path": "a", "expected_hash": readHash(t, secondFixture, "a"), "old_text": "v1", "new_text": "v2",
	})
	records, err := stores.snapshots.List(t.Context())
	if err != nil || len(records) != 2 {
		t.Fatalf("records = %+v, %v", records, err)
	}
	if records[0].Credential.Generation == records[1].Credential.Generation {
		t.Fatalf("generations reused across runs: %+v", records[0].Credential)
	}
	if diskGet(t, secondFixture, "a") != "v2" {
		t.Fatal("second run edit did not apply")
	}
}
