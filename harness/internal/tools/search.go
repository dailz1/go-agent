package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/dailz1/go-agent/harness/internal/workspace"
	"github.com/dailz1/go-agent/tool"
)

type Match struct {
	Path    string `json:"path"`
	Line    int    `json:"line"`
	Text    string `json:"text"`
	Before  []Line `json:"before,omitempty"`
	After   []Line `json:"after,omitempty"`
	Clipped bool   `json:"clipped,omitempty"`
}

type SearchObservation struct {
	Paths      []string `json:"paths"`
	Matches    []Match  `json:"matches"`
	RulePaths  []string `json:"rule_paths"`
	Truncated  bool     `json:"truncated"`
	Reason     string   `json:"reason,omitempty"`
	NextOffset int      `json:"next_offset,omitempty"`
}

type searchArgs struct {
	Pattern    string `json:"pattern"`
	Glob       string `json:"glob"`
	IgnoreCase bool   `json:"ignore_case"`
	Context    int    `json:"context"`
	Offset     int    `json:"offset"`
	Limit      int    `json:"limit"`
}

func (f *Files) glob(ctx context.Context, raw json.RawMessage) (*tool.ToolResult, error) {
	args := struct {
		Pattern string `json:"pattern"`
		Offset  int    `json:"offset"`
		Limit   int    `json:"limit"`
	}{Offset: 1, Limit: 200}
	if err := arguments(raw, &args, "pattern"); err != nil {
		return soft(err)
	}
	return f.search(ctx, searchArgs{Glob: args.Pattern, Offset: args.Offset, Limit: args.Limit}, nil)
}

func (f *Files) grep(ctx context.Context, raw json.RawMessage) (*tool.ToolResult, error) {
	args := searchArgs{Glob: "**", Offset: 1, Limit: 200}
	if err := arguments(raw, &args, "pattern"); err != nil {
		return soft(err)
	}
	if args.Context < 0 || args.Context > 10 {
		return soft(errors.New("context must be between 0 and 10"))
	}
	if len(args.Pattern) > 4096 {
		return soft(errors.New("regexp exceeds 4096 bytes"))
	}
	pattern := args.Pattern
	if args.IgnoreCase {
		pattern = "(?i)" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return soft(err)
	}
	return f.search(ctx, args, re)
}

func (f *Files) search(ctx context.Context, args searchArgs, re *regexp.Regexp) (*tool.ToolResult, error) {
	if err := page(args.Offset, args.Limit); err != nil {
		return soft(err)
	}
	if err := validateGlob(args.Glob); err != nil {
		return soft(err)
	}
	scan, cancel := context.WithTimeout(ctx, f.options.ScanDuration)
	defer cancel()
	result := SearchObservation{Paths: []string{}, Matches: []Match{}, RulePaths: []string{}}
	paths := []string{}
	count := 0
	err := fs.WalkDir(f.workspace.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if scan.Err() != nil {
			result.Truncated, result.Reason = true, "scan time budget reached"
			return fs.SkipAll
		}
		if walkErr != nil {
			result.Truncated, result.Reason = true, "some paths could not be scanned"
			return nil
		}
		count++
		if count > f.options.ScanPaths {
			result.Truncated, result.Reason = true, "path scan budget reached"
			return fs.SkipAll
		}
		excluded := workspace.Sensitive(name) || slices.Contains(f.options.Excludes, entry.Name())
		if entry.Type()&fs.ModeSymlink != 0 || excluded {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type().IsRegular() && matchGlob(args.Glob, name) {
			paths = append(paths, name)
		}
		return nil
	})
	if err != nil {
		return soft(err)
	}
	slices.Sort(paths)
	var scanned int64
	seen, returned, bytes := 0, 0, 0
	hints := map[string]bool{}
	for _, name := range paths {
		if scan.Err() != nil {
			result.Truncated, result.Reason = true, "scan time budget reached"
			break
		}
		matches := []Match{}
		if re != nil {
			info, err := f.workspace.Stat(name)
			if err != nil {
				result.Truncated, result.Reason = true, "some files could not be read"
				continue
			}
			if info.Size() > f.options.ScanBytes-scanned {
				result.Truncated, result.Reason = true, "text scan byte budget reached"
				break
			}
			data, _, err := f.workspace.Read(name, min(workspace.MaxFileBytes, f.options.ScanBytes-scanned))
			if err != nil {
				result.Truncated, result.Reason = true, "some files are oversized, non-text or unreadable"
				continue
			}
			scanned += int64(len(data))
			lines := textLines(string(data))
			for i, line := range lines {
				if scan.Err() != nil {
					break
				}
				if re.MatchString(line) {
					if seen < args.Offset-1 {
						seen++
						continue
					}
					matches = append(matches, matchAt(name, lines, i, args.Context))
					// Only retain enough matches for this page plus one lookahead.
					if len(matches) > args.Limit-returned {
						break
					}
				}
			}
		} else {
			matches = append(matches, Match{Path: name})
		}
		for _, match := range matches {
			seen++
			if seen < args.Offset {
				continue
			}
			encoded, err := json.Marshal(match)
			if err != nil {
				return nil, err
			}
			if len(encoded) > observationBytes {
				match.Before, match.After, match.Clipped = nil, nil, true
				encoded, err = json.Marshal(match)
				if err != nil {
					return nil, err
				}
			}
			if returned == args.Limit || bytes+len(encoded) > observationBytes {
				result.Truncated, result.Reason = true, "result page limit reached"
				result.NextOffset = args.Offset + returned
				return observation(result)
			}
			if re == nil {
				result.Paths = append(result.Paths, name)
			} else {
				result.Matches = append(result.Matches, match)
			}
			returned++
			bytes += len(encoded)
			if match.Clipped {
				result.Truncated, result.Reason = true, "matched lines clipped; use read for full text"
			}
			rules, err := f.rules.ForPath(name)
			if err != nil {
				result.Truncated, result.Reason = true, "applicable rules could not be loaded"
				continue
			}
			for _, source := range rules.Sources {
				if !hints[source.Path] {
					if bytes+len(source.Path)+4 > observationBytes {
						result.Truncated, result.Reason = true, "rule hints clipped; use read for applicable rules"
						continue
					}
					hints[source.Path] = true
					result.RulePaths = append(result.RulePaths, source.Path)
					bytes += len(source.Path) + 4
				}
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if scan.Err() != nil {
		result.Truncated, result.Reason = true, "scan time budget reached"
	}
	return observation(result)
}

func validateGlob(pattern string) error {
	if pattern == "" || len(pattern) > 4096 || strings.HasPrefix(pattern, "/") {
		return errors.New("glob must be workspace-relative and non-empty")
	}
	for _, part := range strings.Split(pattern, "/") {
		if part == ".." || part == "" || (strings.Contains(part, "**") && part != "**") {
			return errors.New("invalid glob segment; ** must occupy a whole segment")
		}
		if part != "**" {
			if _, err := path.Match(part, ""); err != nil {
				return err
			}
		}
	}
	return nil
}

func matchGlob(pattern, name string) bool {
	patterns, names := strings.Split(pattern, "/"), strings.Split(name, "/")
	memo := map[[2]int]bool{}
	visited := map[[2]int]bool{}
	var match func(int, int) bool
	match = func(p, n int) (result bool) {
		key := [2]int{p, n}
		if visited[key] {
			return memo[key]
		}
		defer func() {
			visited[key], memo[key] = true, result
		}()
		if p == len(patterns) {
			return n == len(names)
		}
		if patterns[p] == "**" {
			return match(p+1, n) || (n < len(names) && match(p, n+1))
		}
		if n == len(names) {
			return false
		}
		ok, _ := path.Match(patterns[p], names[n])
		return ok && match(p+1, n+1)
	}
	return match(0, 0)
}

func matchAt(name string, lines []string, index, contextLines int) Match {
	result := Match{Path: name, Line: index + 1}
	clip := func(text string) string {
		text = strings.TrimSuffix(text, "\r")
		if len(text) > 1024 {
			result.Clipped = true
			text = text[:1024]
			for !utf8.ValidString(text) {
				text = text[:len(text)-1]
			}
		}
		return text
	}
	result.Text = clip(lines[index])
	for i := max(0, index-contextLines); i < index; i++ {
		result.Before = append(result.Before, Line{Number: i + 1, Text: clip(lines[i])})
	}
	for i := index + 1; i < min(len(lines), index+1+contextLines); i++ {
		result.After = append(result.After, Line{Number: i + 1, Text: clip(lines[i])})
	}
	return result
}
