package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"

	"github.com/dailz1/go-agent/harness/internal/snapshot"
	"github.com/dailz1/go-agent/harness/internal/workspace"
	"github.com/dailz1/go-agent/tool"
)

func (f *Files) edit(ctx context.Context, raw json.RawMessage) (*tool.ToolResult, error) {
	args := struct {
		Path         string `json:"path"`
		ExpectedHash string `json:"expected_hash"`
		Old          string `json:"old_text"`
		New          string `json:"new_text"`
	}{}
	if err := arguments(raw, &args, "path", "expected_hash", "old_text", "new_text"); err != nil {
		return soft(err)
	}
	if args.Old == "" || args.ExpectedHash == "" {
		return soft(errors.New("edit requires non-empty old_text and expected_hash"))
	}
	change, err := f.prepare(args.Path, args.ExpectedHash)
	if err != nil {
		return soft(err)
	}
	// Count overlapping occurrences too: "aa" in "aaa" is ambiguous.
	first := strings.Index(change.Before, args.Old)
	if first < 0 || strings.Contains(change.Before[first+1:], args.Old) {
		return soft(errors.New("old_text must occur exactly once"))
	}
	change.After = change.Before[:first] + args.New + change.Before[first+len(args.Old):]
	return f.commit(ctx, change)
}

func (f *Files) write(ctx context.Context, raw json.RawMessage) (*tool.ToolResult, error) {
	args := struct {
		Path         string `json:"path"`
		ExpectedHash string `json:"expected_hash"`
		Text         string `json:"text"`
	}{}
	if err := arguments(raw, &args, "path", "text"); err != nil {
		return soft(err)
	}
	change, err := f.prepare(args.Path, args.ExpectedHash)
	if err != nil {
		return soft(err)
	}
	change.After = args.Text
	return f.commit(ctx, change)
}

func (f *Files) prepare(path, expectedHash string) (workspace.Change, error) {
	change := workspace.Change{Path: path, Mode: 0644}
	if err := f.workspace.Writable(path); err != nil {
		return change, err
	}
	if err := f.checkRules(path); err != nil {
		return change, err
	}
	data, info, err := f.workspace.Read(path, workspace.MaxFileBytes)
	if errors.Is(err, os.ErrNotExist) {
		if expectedHash != "" {
			return change, errors.New("expected existing file; read again")
		}
		return change, nil
	}
	if err != nil {
		return change, err
	}
	if expectedHash == "" || expectedHash != workspace.Hash(data) {
		return change, errors.New("expected_hash does not match current file; read again")
	}
	change.Exists, change.Before, change.Mode = true, string(data), info.Mode().Perm()
	return change, nil
}

func (f *Files) checkRules(path string) error {
	if f.rules.RootChanged() {
		return errors.New("root rules changed; start a new session to adopt them")
	}
	current, err := f.rules.ForPath(path)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, version := range f.observed {
		if version == current.Version {
			return nil
		}
	}
	return errors.New("read the target or parent directory to load current project rules")
}

func (f *Files) commit(ctx context.Context, change workspace.Change) (*tool.ToolResult, error) {
	if len(change.After) > workspace.MaxFileBytes || !workspace.Text([]byte(change.After)) {
		return soft(errors.New("modified file must be UTF-8 text at most 2 MiB"))
	}
	if f.options.Writes == nil {
		return soft(errors.New("writes disabled: approval and durable snapshot gate is not connected"))
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	changed := !change.Exists || change.Before != change.After
	if changed {
		applied := false
		var precondition error
		err := f.options.Writes.Commit(ctx, change, func() error {
			if applied {
				return errors.New("write gate invoked apply more than once")
			}
			if err := f.checkRules(change.Path); err != nil {
				precondition = err
				return err
			}
			if err := f.workspace.Verify(change); err != nil {
				precondition = err
				return err
			}
			if err := f.workspace.Replace(ctx, change); err != nil {
				return err
			}
			applied = true
			return nil
		})
		if err != nil {
			if errors.Is(err, ErrDenied) || errors.Is(err, snapshot.ErrConflict) || precondition != nil {
				return soft(err)
			}
			return nil, err
		}
		if !applied {
			return nil, errors.New("write gate returned without applying the change")
		}
	}
	return observation(struct {
		Path    string `json:"path"`
		Hash    string `json:"hash"`
		Changed bool   `json:"changed"`
	}{Path: change.Path, Hash: workspace.Hash([]byte(change.After)), Changed: changed})
}
