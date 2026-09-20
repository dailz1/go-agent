// Package agenttool adapts a preconstructed Agent as a tool for another Agent.
//
// Import github.com/dailz1/go-agent/agenttool. New exposes caller-owned Config
// metadata with a fixed input:string schema. Each Execute is stateless: it makes
// one child Run and returns only value-form final
// TextBlocks. Malformed input, a truncated child run, and a textless final result
// are soft errors; child Run and context errors are hard errors. Each returned
// adapter serializes its own calls with a context-aware gate. Adapters created
// separately for the same child do not interlock; callers must ensure shared
// providers, compactors, stores, registries, and child tools are concurrency-safe.
// Child sessions, Store thread IDs, and events are not forwarded.
package agenttool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dailz1/go-agent/agent"
	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

// Config describes the tool-facing identity exposed to the parent Agent.
type Config struct {
	// Name is the tool name registered with the parent Agent.
	Name string
	// Description tells the parent Agent when to delegate to this child.
	Description string
	// RequiresApproval delegates outer approval to the parent Agent.
	RequiresApproval bool
}

type adapter struct {
	child *agent.Agent
	info  tool.ToolInfo
	gate  chan struct{}
}

// New returns a Tool that delegates one free-text task to child per Execute.
func New(child *agent.Agent, cfg Config) tool.Tool {
	parameters := tool.NewParameterSchema()
	parameters.Properties["input"] = tool.Param("string", "完整交给子 agent 的任务")
	parameters.Required = []string{"input"}
	return &adapter{
		child: child,
		info: tool.ToolInfo{
			Name:             cfg.Name,
			Description:      cfg.Description,
			Parameters:       parameters,
			RequiresApproval: cfg.RequiresApproval,
		},
		gate: make(chan struct{}, 1),
	}
}

func (a *adapter) Info() tool.ToolInfo { return a.info }

func (a *adapter) Execute(ctx context.Context, args json.RawMessage) (*tool.ToolResult, error) {
	var input struct {
		Input *string `json:"input"`
	}
	if err := json.Unmarshal(args, &input); err != nil || input.Input == nil {
		return tool.NewErrorResult("invalid arguments: expected object with string input"), nil
	}
	select {
	case a.gate <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-a.gate }()

	result, err := a.child.Run(ctx, *input.Input)
	if err != nil {
		return nil, fmt.Errorf("run sub-agent %q: %w", a.info.Name, err)
	}
	if result.Truncated {
		return tool.NewErrorResult("sub-agent %q reached its iteration limit without a reliable final answer", a.info.Name), nil
	}
	var text strings.Builder
	foundText := false
	for _, block := range result.Message.Content {
		if block, ok := block.(llm.TextBlock); ok {
			foundText = true
			text.WriteString(block.Text)
		}
	}
	if !foundText {
		return tool.NewErrorResult("sub-agent %q returned no final text", a.info.Name), nil
	}
	return tool.NewTextResult(text.String()), nil
}
