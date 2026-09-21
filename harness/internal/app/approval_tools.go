package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/dailz1/go-agent/harness/internal/tools"
	"github.com/dailz1/go-agent/harness/internal/workspace"
	"github.com/dailz1/go-agent/tool"
)

type approvalCall struct {
	name string
	args string
}

type approvalCallKey struct{}

type runTool struct {
	tool.Tool
	run *RunApproval
}

func (t runTool) Execute(ctx context.Context, raw json.RawMessage) (*tool.ToolResult, error) {
	t.run.manager.mu.Lock()
	err := t.run.check(ctx)
	t.run.manager.mu.Unlock()
	if err != nil {
		return nil, err
	}
	// The owned run context prevents a caller substituting a fresh context after
	// cancellation. Its cancellation is combined with the caller's context.
	callCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(t.run.ctx, cancel)
	defer stop()
	defer cancel()
	call := approvalCall{name: t.Info().Name, args: string(raw)}
	return t.Tool.Execute(context.WithValue(callCtx, approvalCallKey{}, call), raw)
}

// ApprovalFn is passed to agent.WithApprovalFn for this run only. The tool gate
// consumes the same exact one-use credential; it does not ask twice.
func (r *RunApproval) ApprovalFn(info tool.ToolInfo, raw json.RawMessage) bool {
	request, err := r.preview(info.Name, string(raw))
	if err != nil {
		return false
	}
	if err := r.authorize(r.ctx, request); err != nil {
		return false
	}
	a := r.manager
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := r.check(r.ctx); err != nil {
		return false
	}
	r.permits[approvalKey(request)]++
	return true
}

func (r *RunApproval) preview(name, raw string) (ApprovalRequest, error) {
	request := ApprovalRequest{Tool: name, Arguments: raw}
	switch name {
	case "edit", "write":
		args := struct {
			Path string `json:"path"`
			Text string `json:"text"`
			Old  string `json:"old_text"`
			New  string `json:"new_text"`
		}{}
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			return request, err
		}
		request.Path = args.Path
		if err := r.workspace.Writable(args.Path); err != nil {
			return request, err
		}
		change := workspace.Change{Path: args.Path, Mode: 0644}
		data, info, err := r.workspace.Read(args.Path, workspace.MaxFileBytes)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return request, err
		}
		if err == nil {
			change.Before, change.Exists, change.Mode = string(data), true, info.Mode().Perm()
		}
		change.After = args.Text
		if name == "edit" {
			change.After = strings.Replace(change.Before, args.Old, args.New, 1)
		}
		request.Change = change
	case "shell":
		args := struct {
			Command string  `json:"command"`
			Cwd     string  `json:"cwd"`
			Timeout float64 `json:"timeout"`
		}{Cwd: ".", Timeout: 120}
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			return request, err
		}
		request.Command, request.Cwd = args.Command, args.Cwd
		request.Timeout = time.Duration(args.Timeout * float64(time.Second))
	default:
		return request, tools.ErrDenied
	}
	return request, nil
}

func approvalKey(request ApprovalRequest) string {
	// JSON gives unambiguous field boundaries, including embedded NUL/newlines.
	data, _ := json.Marshal(request)
	return string(data)
}

func (r *RunApproval) require(ctx context.Context, request ApprovalRequest) error {
	if call, ok := ctx.Value(approvalCallKey{}).(approvalCall); ok {
		if call.name != request.Tool {
			return tools.ErrDenied
		}
		request.Arguments = call.args
	}
	a := r.manager
	a.mu.Lock()
	if err := r.check(ctx); err != nil {
		a.mu.Unlock()
		return err
	}
	if a.grant.paths != nil && !a.now().Before(a.grant.expires) {
		a.grant = fileGrant{}
		r.permits = map[string]int{}
	}
	key := approvalKey(request)
	if r.permits[key] > 0 {
		r.permits[key]--
		a.mu.Unlock()
		return nil
	}
	a.mu.Unlock()
	return r.authorize(ctx, request)
}

func (r *RunApproval) ApproveRead(ctx context.Context, path string) error {
	return r.require(ctx, ApprovalRequest{Tool: "read", Path: path})
}

func (r *RunApproval) ApproveShell(ctx context.Context, command, cwd string, timeout time.Duration) error {
	return r.require(ctx, ApprovalRequest{Tool: "shell", Command: command, Cwd: cwd, Timeout: timeout})
}

func (r *RunApproval) approveChange(ctx context.Context, change workspace.Change) error {
	call, ok := ctx.Value(approvalCallKey{}).(approvalCall)
	if !ok || (call.name != "write" && call.name != "edit") {
		return fmt.Errorf("missing write invocation identity: %w", tools.ErrDenied)
	}
	return r.require(ctx, ApprovalRequest{Tool: call.name, Path: change.Path, Change: change})
}
