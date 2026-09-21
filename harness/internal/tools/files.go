package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/dailz1/go-agent/harness/internal/prompt"
	"github.com/dailz1/go-agent/harness/internal/workspace"
	"github.com/dailz1/go-agent/tool"
)

var ErrDenied = errors.New("operation denied")

// WriteGate is supplied by Stage C. Commit must obtain valid approval and
// durably prepare the before/after snapshot BEFORE invoking apply, exactly once.
// It records the applied state after apply returns. Denial wraps ErrDenied;
// snapshot/persistence failures remain Go errors. Nil means writes are disabled.
type WriteGate interface {
	Commit(context.Context, workspace.Change, func() error) error
}

type ReadApproval interface {
	ApproveRead(context.Context, string) error
}

// OutputReader resolves opaque IDs, never host paths. Stage C owns storage.
type OutputReader interface {
	ReadOutput(context.Context, string, int, int) (ReadObservation, error)
}

type Options struct {
	Writes       WriteGate
	Sensitive    ReadApproval
	Outputs      OutputReader
	Excludes     []string
	ScanPaths    int
	ScanBytes    int64
	ScanDuration time.Duration
}

// Files is scoped to one session: observed rules must not leak across sessions.
type Files struct {
	workspace *workspace.Workspace
	rules     *prompt.Rules
	options   Options
	mu        sync.Mutex
	observed  map[string]string
}

func New(w *workspace.Workspace, rules *prompt.Rules, options Options) *Files {
	if options.Excludes == nil {
		options.Excludes = []string{".git", "node_modules", "vendor", "build", "dist", "target"}
	}
	if options.ScanPaths <= 0 {
		options.ScanPaths = 100000
	}
	if options.ScanBytes <= 0 {
		options.ScanBytes = 64 << 20
	}
	if options.ScanDuration <= 0 {
		options.ScanDuration = 10 * time.Second
	}
	return &Files{workspace: w, rules: rules, options: options, observed: map[string]string{}}
}

type fileTool struct {
	info    tool.ToolInfo
	execute func(context.Context, json.RawMessage) (*tool.ToolResult, error)
}

func (t fileTool) Info() tool.ToolInfo { return t.info }
func (t fileTool) Execute(ctx context.Context, args json.RawMessage) (*tool.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result, err := t.execute(ctx, args)
	if err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func arguments(raw json.RawMessage, value any, required ...string) error {
	if len(raw) > 12<<20 {
		return errors.New("arguments exceed byte limit")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(value); err != nil {
		return errors.New("invalid arguments or unknown field")
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return errors.New("expected one JSON object")
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return errors.New("expected a JSON object")
	}
	for _, field := range fields {
		if bytes.Equal(bytes.TrimSpace(field), []byte("null")) {
			return errors.New("null arguments are not supported")
		}
	}
	for _, key := range required {
		if _, ok := fields[key]; !ok {
			return errors.New("missing required argument: " + key)
		}
	}
	return nil
}

func observation(value any) (*tool.ToolResult, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return tool.NewTextResult(string(data)), nil
}

func soft(err error) (*tool.ToolResult, error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil, err
	}
	return tool.NewErrorResult("%v", err), nil
}

func page(offset, limit int) error {
	if offset < 1 || limit < 1 || limit > 1000 {
		return errors.New("offset must be positive; limit must be between 1 and 1000")
	}
	return nil
}
