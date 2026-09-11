// Filesystem and shell tools for the agentcli demo. They are deliberately
// small: the point is to show the library's tool surface — schema-driven
// parameters, soft-error results, and the approval gate — not to be a
// hardened automation toolkit.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/dailz1/go-agent/pkg/tool"
)

func registerTools(r *tool.Registry) {
	r.MustRegister(newReadFile())
	r.MustRegister(newWriteFile())
	r.MustRegister(newListDir())
	r.MustRegister(newRunShell())
}

func schema(props map[string]tool.Property, required ...string) tool.ParameterSchema {
	return tool.ParameterSchema{Type: "object", Properties: props, Required: required}
}

// --- read_file ---

type readFileTool struct{}

func newReadFile() tool.Tool { return readFileTool{} }

func (readFileTool) Info() tool.ToolInfo {
	return tool.ToolInfo{
		Name:        "read_file",
		Description: "Read a text file and return its full content.",
		Parameters:  schema(map[string]tool.Property{"path": tool.Param("string", "file path")}, "path"),
	}
}

func (readFileTool) Execute(_ context.Context, args json.RawMessage) (*tool.ToolResult, error) {
	var p struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return tool.NewErrorResult("invalid arguments: %v", err), nil
	}
	data, err := os.ReadFile(p.Path)
	if err != nil {
		return tool.NewErrorResult("read %s: %v", p.Path, err), nil
	}
	return tool.NewTextResult(string(data)), nil
}

// --- write_file ---

type writeFileTool struct{}

func newWriteFile() tool.Tool { return writeFileTool{} }

func (writeFileTool) Info() tool.ToolInfo {
	return tool.ToolInfo{
		Name:        "write_file",
		Description: "Create or overwrite a text file with the given content. Parent directories are created automatically.",
		Parameters: schema(map[string]tool.Property{
			"path":    tool.Param("string", "file path"),
			"content": tool.Param("string", "full file content"),
		}, "path", "content"),
	}
}

func (writeFileTool) Execute(_ context.Context, args json.RawMessage) (*tool.ToolResult, error) {
	var p struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return tool.NewErrorResult("invalid arguments: %v", err), nil
	}
	if dir := filepath.Dir(p.Path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return tool.NewErrorResult("create %s: %v", dir, err), nil
		}
	}
	if err := os.WriteFile(p.Path, []byte(p.Content), 0o644); err != nil {
		return tool.NewErrorResult("write %s: %v", p.Path, err), nil
	}
	return tool.NewTextResult(fmt.Sprintf("wrote %s (%d bytes)", p.Path, len(p.Content))), nil
}

// --- list_dir ---

type listDirTool struct{}

func newListDir() tool.Tool { return listDirTool{} }

func (listDirTool) Info() tool.ToolInfo {
	return tool.ToolInfo{
		Name:        "list_dir",
		Description: "List the entries of a directory with their types and sizes.",
		Parameters:  schema(map[string]tool.Property{"path": tool.Param("string", "directory path")}, "path"),
	}
}

func (listDirTool) Execute(_ context.Context, args json.RawMessage) (*tool.ToolResult, error) {
	var p struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return tool.NewErrorResult("invalid arguments: %v", err), nil
	}
	entries, err := os.ReadDir(p.Path)
	if err != nil {
		return tool.NewErrorResult("list %s: %v", p.Path, err), nil
	}
	if len(entries) == 0 {
		return tool.NewTextResult("(empty)"), nil
	}
	out := ""
	for _, e := range entries {
		if e.IsDir() {
			out += e.Name() + "/\n"
			continue
		}
		size := "-"
		if info, err := e.Info(); err == nil {
			size = fmt.Sprintf("%d", info.Size())
		}
		out += fmt.Sprintf("%s (%s bytes)\n", e.Name(), size)
	}
	return tool.NewTextResult(out), nil
}

// --- run_shell ---

const shellTimeout = 60 * time.Second

type runShellTool struct{}

func newRunShell() tool.Tool { return runShellTool{} }

func (runShellTool) Info() tool.ToolInfo {
	return tool.ToolInfo{
		Name:        "run_shell",
		Description: "Run a shell command (sh -c) in the current directory and return its combined output. Every invocation requires the user's approval.",
		Parameters:  schema(map[string]tool.Property{"command": tool.Param("string", "shell command to run")}, "command"),
		// The approval callback in main.go gates every execution.
		RequiresApproval: true,
	}
}

func (runShellTool) Execute(ctx context.Context, args json.RawMessage) (*tool.ToolResult, error) {
	var p struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return tool.NewErrorResult("invalid arguments: %v", err), nil
	}
	ctx, cancel := context.WithTimeout(ctx, shellTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", p.Command)
	out, err := cmd.CombinedOutput()
	result := fmt.Sprintf("$ %s\n%s", p.Command, out)
	if ctx.Err() != nil {
		return tool.NewErrorResult("%s\n(timed out after %s)", result, shellTimeout), nil
	}
	if err != nil {
		// A non-zero exit is a tool-level outcome for the model to react to,
		// not a system failure: report it softly.
		return tool.NewErrorResult("%s\n(exit %v)", result, err), nil
	}
	return tool.NewTextResult(result), nil
}
