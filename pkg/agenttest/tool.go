package agenttest

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/dailz1/go-agent/pkg/tool"
)

// ToolCall is an immutable snapshot of arguments supplied to ToolFunc.
type ToolCall struct {
	Args json.RawMessage
}

// ToolFunc is a minimal, race-safe tool test double.
type ToolFunc struct {
	Definition  tool.ToolInfo
	ExecuteFunc func(context.Context, json.RawMessage) (*tool.ToolResult, error)

	mu    sync.Mutex
	calls []ToolCall
}

func (t *ToolFunc) Info() tool.ToolInfo { return t.Definition }

func (t *ToolFunc) Execute(ctx context.Context, args json.RawMessage) (*tool.ToolResult, error) {
	copyArgs := append(json.RawMessage(nil), args...)
	t.mu.Lock()
	t.calls = append(t.calls, ToolCall{Args: copyArgs})
	t.mu.Unlock()
	if t.ExecuteFunc == nil {
		return nil, errors.New("agenttest ToolFunc has no ExecuteFunc")
	}
	return t.ExecuteFunc(ctx, copyArgs)
}

func (t *ToolFunc) Calls() []ToolCall {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]ToolCall, len(t.calls))
	for i, call := range t.calls {
		out[i].Args = append(json.RawMessage(nil), call.Args...)
	}
	return out
}
