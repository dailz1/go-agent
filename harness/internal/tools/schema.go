package tools

import (
	"github.com/dailz1/go-agent/tool"
)

// Registrar receives the workspace tools; *tool.Registry satisfies it. Hosts
// may wrap each tool before it reaches the agent-consumed registry.
type Registrar interface {
	Register(tool.Tool) error
}

func (f *Files) Register(registry Registrar) error {
	pagination := map[string]tool.Property{
		"offset": tool.Param("integer", "1-based starting line or result; default 1"),
		"limit":  tool.Param("integer", "Maximum lines or results, 1..1000; default 200"),
	}
	read := map[string]tool.Property{
		"path":      tool.Param("string", "Workspace-relative file or target path; a missing file returns its parent rules"),
		"output_id": tool.Param("string", "Opaque saved shell output ID, mutually exclusive with path"),
	}
	glob := map[string]tool.Property{
		"pattern": tool.Param("string", "Root-relative glob; *, ?, [], and whole-segment **"),
	}
	grep := map[string]tool.Property{
		"pattern":     tool.Param("string", "Go regular expression (not PCRE)"),
		"glob":        tool.Param("string", "Optional root-relative file glob"),
		"ignore_case": tool.Param("boolean", "Case-insensitive matching; default false"),
		"context":     tool.Param("integer", "Context lines before and after, 0..10"),
	}
	for key, property := range pagination {
		read[key], glob[key], grep[key] = property, property, property
	}
	tools := []fileTool{
		{
			info:    info("read", "Read numbered UTF-8 lines, content hash and applicable project rules.", read, nil, false),
			execute: f.read,
		},
		{
			info:    info("glob", "Find sorted workspace paths and rule-file hints with bounded scanning.", glob, []string{"pattern"}, false),
			execute: f.glob,
		},
		{
			info:    info("grep", "Search workspace UTF-8 text, with line numbers and explicit incomplete results.", grep, []string{"pattern"}, false),
			execute: f.grep,
		},
		{
			info: info("edit", "Replace exactly one occurrence after hash and rules checks; requires approval and snapshot.", map[string]tool.Property{
				"path":          tool.Param("string", "Single workspace-relative file"),
				"expected_hash": tool.Param("string", "Hash returned by read"),
				"old_text":      tool.Param("string", "Non-empty exact text occurring once"),
				"new_text":      tool.Param("string", "Replacement UTF-8 text"),
			}, []string{"path", "expected_hash", "old_text", "new_text"}, true),
			execute: f.edit,
		},
		{
			info: info("write", "Create or replace one complete UTF-8 file; requires approval and snapshot.", map[string]tool.Property{
				"path":          tool.Param("string", "Single workspace-relative file"),
				"expected_hash": tool.Param("string", "Required to overwrite; omit for a new file"),
				"text":          tool.Param("string", "Complete file contents, at most 2 MiB"),
			}, []string{"path", "text"}, true),
			execute: f.write,
		},
	}
	for _, impl := range tools {
		if err := registry.Register(impl); err != nil {
			return err
		}
	}
	return nil
}

func info(name, description string, properties map[string]tool.Property, required []string, approval bool) tool.ToolInfo {
	noExtra := false
	return tool.ToolInfo{
		Name: name, Description: description, RequiresApproval: approval,
		Parameters: tool.ParameterSchema{
			Type: "object", Properties: properties, Required: required, AdditionalProperties: &noExtra,
		},
	}
}
