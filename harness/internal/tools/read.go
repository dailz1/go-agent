package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/dailz1/go-agent/harness/internal/prompt"
	"github.com/dailz1/go-agent/harness/internal/workspace"
	"github.com/dailz1/go-agent/tool"
)

const observationBytes = 32 << 10

type Line struct {
	Number int    `json:"number"`
	Text   string `json:"text"`
}

type ReadObservation struct {
	Path        string            `json:"path,omitempty"`
	OutputID    string            `json:"output_id,omitempty"`
	Exists      bool              `json:"exists"`
	Directory   bool              `json:"directory,omitempty"`
	Hash        string            `json:"hash,omitempty"`
	Lines       []Line            `json:"lines"`
	Truncated   bool              `json:"truncated"`
	NextOffset  int               `json:"next_offset,omitempty"`
	Rules       prompt.Applicable `json:"rules"`
	RootChanged bool              `json:"root_rules_changed,omitempty"`
}

func (f *Files) read(ctx context.Context, raw json.RawMessage) (*tool.ToolResult, error) {
	args := struct {
		Path     string `json:"path"`
		OutputID string `json:"output_id"`
		Offset   int    `json:"offset"`
		Limit    int    `json:"limit"`
	}{Offset: 1, Limit: 200}
	if err := arguments(raw, &args); err != nil {
		return soft(err)
	}
	if err := page(args.Offset, args.Limit); err != nil {
		return soft(err)
	}
	if (args.Path == "") == (args.OutputID == "") {
		return soft(errors.New("provide exactly one of path or output_id"))
	}
	if args.OutputID != "" {
		if !regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`).MatchString(args.OutputID) {
			return soft(errors.New("invalid opaque output ID"))
		}
		if f.options.Outputs == nil {
			return soft(errors.New("saved output reader is not connected"))
		}
		result, err := f.options.Outputs.ReadOutput(ctx, args.OutputID, args.Offset, args.Limit)
		if err != nil {
			return soft(err)
		}
		return observation(result)
	}
	if err := f.workspace.Check(args.Path); err != nil {
		return soft(err)
	}
	if workspace.Sensitive(args.Path) {
		if f.options.Sensitive == nil {
			return soft(errors.New("sensitive file requires explicit read approval"))
		}
		if err := f.options.Sensitive.ApproveRead(ctx, args.Path); err != nil {
			return soft(err)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rules, err := f.rules.ForPath(args.Path)
	if err != nil {
		return soft(err)
	}
	result := ReadObservation{
		Path: filepath.Clean(args.Path), Lines: []Line{}, Rules: rules,
		RootChanged: f.rules.RootChanged(),
	}
	info, err := f.workspace.Stat(args.Path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return soft(err)
	}
	if err == nil {
		result.Exists, result.Directory = true, info.IsDir()
		if !info.IsDir() {
			data, _, err := f.workspace.Read(args.Path, workspace.MaxFileBytes)
			if err != nil {
				return soft(err)
			}
			result.Hash = workspace.Hash(data)
			result.Lines, result.Truncated = numberedLines(string(data), args.Offset, args.Limit)
			if result.Truncated {
				result.NextOffset = args.Offset + len(result.Lines)
			}
		}
	}
	f.mu.Lock()
	f.observed[result.Path] = rules.Version
	f.mu.Unlock()
	return observation(result)
}

func textLines(text string) []string {
	if text == "" {
		return []string{}
	}
	return strings.Split(strings.TrimSuffix(text, "\n"), "\n")
}

func numberedLines(text string, offset, limit int) ([]Line, bool) {
	lines := textLines(text)
	result := []Line{}
	size := 0
	for i := offset - 1; i < len(lines); i++ {
		if len(result) == limit || size >= observationBytes {
			return result, true
		}
		line := strings.TrimSuffix(lines[i], "\r")
		remaining := observationBytes - size
		if len(line) > remaining {
			line = line[:remaining]
			for !utf8.ValidString(line) {
				line = line[:len(line)-1]
			}
			result = append(result, Line{Number: i + 1, Text: line})
			return result, true
		}
		result = append(result, Line{Number: i + 1, Text: line})
		size += len(line) + 32
	}
	return result, false
}
