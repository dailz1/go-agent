//go:build linux

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"github.com/dailz1/go-agent/harness/internal/workspace"
	"github.com/dailz1/go-agent/tool"
)

type ShellApproval interface {
	ApproveShell(context.Context, string, string, time.Duration) error
}

type ShellOption func(*Shell)

// WithShellInlineBudget bounds the combined head/tail bytes, not the JSON
// envelope. Callers must reserve room for the output ID and exit facts.
func WithShellInlineBudget(bytes int) ShellOption {
	return func(s *Shell) { s.inline = max(0, min(bytes, maxInlineBytes)) }
}

// Shell serializes launches and permanently refuses new work after cleanup
// cannot be confirmed. A new executor must not be used to bypass that failure.
type Shell struct {
	workspace     *workspace.Workspace
	approval      ShellApproval
	outputs       *OutputStore
	credentialEnv map[string]bool
	inline        int
	slot          chan struct{}
	cleanupErr    *CleanupError
	termGrace     time.Duration
	killGrace     time.Duration
	groupAlive    func(int) (bool, error)
}

func NewShell(
	w *workspace.Workspace,
	approval ShellApproval,
	outputs *OutputStore,
	credentialEnv []string,
	options ...ShellOption,
) *Shell {
	s := &Shell{
		workspace: w, approval: approval, outputs: outputs, inline: maxInlineBytes,
		credentialEnv: map[string]bool{}, slot: make(chan struct{}, 1),
		termGrace: 2 * time.Second, killGrace: 2 * time.Second, groupAlive: processGroupAlive,
	}
	for _, name := range credentialEnv {
		s.credentialEnv[name] = true
	}
	for _, option := range options {
		option(s)
	}
	return s
}

type ShellObservation struct {
	OutputID  string `json:"output_id"`
	Outcome   string `json:"outcome"`
	ExitCode  int    `json:"exit_code"`
	Bytes     int64  `json:"bytes"`
	Truncated bool   `json:"truncated"`
	Head      string `json:"head"`
	Tail      string `json:"tail"`
}

type CleanupError struct {
	PGID  int
	Cause error
}

func (e *CleanupError) Error() string {
	return fmt.Sprintf("shell process group %d cleanup unconfirmed: %v", e.PGID, e.Cause)
}

func (e *CleanupError) Unwrap() error { return e.Cause }

func (s *Shell) Info() tool.ToolInfo {
	return info("shell", "Run an approved Linux foreground command; saved output is bounded and paginated with read.", map[string]tool.Property{
		"command": tool.Param("string", "Command passed to /bin/sh -c; stdin is EOF"),
		"cwd":     tool.Param("string", "Workspace-relative directory; default ."),
		"timeout": tool.Param("number", "Timeout in seconds, greater than 0 and at most 600; default 120"),
	}, []string{"command"}, true)
}

func (s *Shell) Execute(ctx context.Context, raw json.RawMessage) (*tool.ToolResult, error) {
	select {
	case s.slot <- struct{}{}:
		defer func() { <-s.slot }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if s.cleanupErr != nil {
		return nil, s.cleanupErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	args := struct {
		Command string  `json:"command"`
		Cwd     string  `json:"cwd"`
		Timeout float64 `json:"timeout"`
	}{Cwd: ".", Timeout: 120}
	if err := arguments(raw, &args, "command"); err != nil {
		return soft(err)
	}
	if strings.TrimSpace(args.Command) == "" || strings.ContainsRune(args.Command, 0) ||
		math.IsNaN(args.Timeout) || args.Timeout <= 0 || args.Timeout > 600 {
		return soft(errors.New("command must be nonempty and timeout must be in (0, 600] seconds"))
	}
	timeout := time.Duration(args.Timeout * float64(time.Second))
	if timeout <= 0 {
		return soft(errors.New("timeout is below one nanosecond"))
	}
	if s.workspace == nil || s.outputs == nil {
		return nil, errors.New("shell workspace and output store are required")
	}
	if err := s.workspace.Check(args.Cwd); err != nil {
		return soft(err)
	}
	rooted, err := s.workspace.FS().Open(args.Cwd)
	if err != nil {
		return soft(err)
	}
	defer rooted.Close()
	dir, ok := rooted.(*os.File)
	if !ok {
		return nil, errors.New("workspace did not return a directory file descriptor")
	}
	stat, err := dir.Stat()
	if err != nil {
		return soft(err)
	}
	if !stat.IsDir() {
		return soft(errors.New("cwd must be a directory"))
	}
	if s.approval == nil {
		return soft(ErrDenied)
	}
	if err := s.approval.ApproveShell(ctx, args.Command, args.Cwd, timeout); err != nil {
		return soft(err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	got, err := s.run(ctx, args.Command, dir, timeout)
	if err != nil && got == nil {
		return nil, err
	}
	result, encodeErr := observation(got)
	if encodeErr != nil {
		return nil, errors.Join(err, encodeErr)
	}
	if got.Outcome != "exit" || got.ExitCode != 0 {
		result.Status = tool.ResultError
	}
	return result, err
}
