//go:build linux

package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dailz1/go-agent/harness/internal/workspace"
	"github.com/dailz1/go-agent/tool"
)

type shellApprovalFunc func(context.Context, string, string, time.Duration) error

func (f shellApprovalFunc) ApproveShell(ctx context.Context, command, cwd string, timeout time.Duration) error {
	return f(ctx, command, cwd, timeout)
}

func shellFixture(t *testing.T) (*Shell, string, *OutputStore) {
	t.Helper()
	dir := t.TempDir()
	w, err := workspace.Open(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	store, err := OpenOutputStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	s := NewShell(w, shellApprovalFunc(func(context.Context, string, string, time.Duration) error {
		return nil
	}), store, []string{"HARNESS_TEST_SECRET"})
	s.termGrace = 30 * time.Millisecond
	return s, dir, store
}

func executeShell(t *testing.T, s *Shell, command string) ShellObservation {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"command": command})
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.Execute(t.Context(), raw)
	if err != nil {
		t.Fatal(err)
	}
	var got ShellObservation
	if err := json.Unmarshal([]byte(result.Content), &got); err != nil {
		t.Fatalf("%s: %v", result.Content, err)
	}
	if (got.Outcome != "exit" || got.ExitCode != 0) != result.IsError() {
		t.Fatalf("status disagrees with outcome: %+v / %+v", got, result)
	}
	return got
}

func TestShellExitStdinCwdEnvironment(t *testing.T) {
	s, dir, _ := shellFixture(t)
	t.Setenv("HARNESS_TEST_SECRET", "hidden")
	t.Setenv("HARNESS_TEST_INHERITED", "visible")
	got := executeShell(t, s, `read value; printf '%s|%s|%s\n' "$PWD" "${HARNESS_TEST_SECRET-unset}" "$HARNESS_TEST_INHERITED"; printf err >&2; exit 7`)
	if got.ExitCode != 7 || got.Outcome != "exit" || !strings.Contains(got.Head, dir+"|unset|visible") ||
		!strings.Contains(got.Head, "err") || got.OutputID == "" {
		t.Fatalf("unexpected observation: %+v", got)
	}
}

func TestShellDenialValidationAndLateApproval(t *testing.T) {
	s, dir, _ := shellFixture(t)
	s.approval = nil
	result, err := s.Execute(t.Context(), json.RawMessage(`{"command":"touch launched"}`))
	if err != nil || !result.IsError() {
		t.Fatalf("nil approval: %v %v", result, err)
	}
	for _, raw := range []string{`{}`, `null`, `{"command":""}`, `{"command":"true","cwd":"../"}`,
		`{"command":"true","timeout":601}`, `{"command":"true","timeout":0}`, `{"command":"true","extra":1}`} {
		result, err := s.Execute(t.Context(), json.RawMessage(raw))
		if err != nil || !result.IsError() {
			t.Fatalf("%s: %v %v", raw, result, err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	s.approval = shellApprovalFunc(func(context.Context, string, string, time.Duration) error {
		cancel()
		return nil
	})
	if _, err := s.Execute(ctx, json.RawMessage(`{"command":"touch launched"}`)); !errors.Is(err, context.Canceled) {
		t.Fatalf("late approval: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "launched")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("command executed: %v", err)
	}
}

func shellFIFO(t *testing.T, dir, name string) *os.File {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	if err := f.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return f
}

func assertProcessStopped(t *testing.T, pid int) {
	t.Helper()
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(data)[strings.LastIndexByte(string(data), ')')+1:])
	if fields[0] != "Z" && fields[0] != "X" {
		t.Fatalf("process %d still live: %s", pid, data)
	}
}

func TestShellCancelGrandchildrenAndNormalBackground(t *testing.T) {
	for _, normal := range []bool{false, true} {
		t.Run(fmt.Sprintf("normal=%v", normal), func(t *testing.T) {
			s, dir, _ := shellFixture(t)
			ready := shellFIFO(t, dir, "ready")
			release := shellFIFO(t, dir, "release")
			shellFIFO(t, dir, "hold")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			command := `/bin/sh -c '/bin/sh -c '"'"'trap "" TERM; echo $$ > ready; read x < hold'"'"' & wait' & read x < release`
			type finished struct {
				result *tool.ToolResult
				err    error
			}
			done := make(chan finished, 1)
			go func() {
				raw, _ := json.Marshal(map[string]string{"command": command})
				result, err := s.Execute(ctx, raw)
				done <- finished{result, err}
			}()
			line, err := bufio.NewReader(ready).ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			pid, err := strconv.Atoi(strings.TrimSpace(line))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })
			if normal {
				if _, err := release.WriteString("exit\n"); err != nil {
					t.Fatal(err)
				}
			} else {
				cancel()
			}
			select {
			case got := <-done:
				if normal && (got.err != nil || got.result.IsError()) {
					t.Fatalf("normal exit: %v %v", got.result, got.err)
				}
				if !normal && !errors.Is(got.err, context.Canceled) {
					t.Fatalf("cancel: %v", got.err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("shell did not join")
			}
			assertProcessStopped(t, pid)
		})
	}
}

func TestShellTimeoutAndCleanupFailureLatch(t *testing.T) {
	s, _, _ := shellFixture(t)
	result, err := s.Execute(t.Context(), json.RawMessage(`{"command":"read x < /dev/zero; while :; do :; done","timeout":0.02}`))
	if err != nil {
		t.Fatal(err)
	}
	var got ShellObservation
	if err := json.Unmarshal([]byte(result.Content), &got); err != nil || got.Outcome != "timeout" || !result.IsError() {
		t.Fatalf("timeout: %+v %v", got, err)
	}
	s.groupAlive = func(int) (bool, error) { return false, errors.New("proc unavailable") }
	_, err = s.Execute(t.Context(), json.RawMessage(`{"command":"true"}`))
	var cleanup *CleanupError
	if !errors.As(err, &cleanup) {
		t.Fatalf("not a cleanup error: %v", err)
	}
	_, second := s.Execute(t.Context(), json.RawMessage(`{"command":"touch forbidden"}`))
	if !errors.As(second, &cleanup) {
		t.Fatalf("not latched: %v", second)
	}
}
