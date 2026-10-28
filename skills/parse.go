package skills

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	// frontmatterFence opens and closes the frontmatter block.
	frontmatterFence = "---"
	// maxNameRunes bounds a skill name; its shape is ruled by skillNamePattern.
	maxNameRunes = 64
	// maxDescriptionRunes caps one catalog entry's description before the ellipsis.
	maxDescriptionRunes = 1024
)

var skillNamePattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// Value modes for a recognized key (name, description) while its value is
// being scanned.
const (
	modeEmptyValue = iota // awaiting the first continuation line, which is classified before folding
	modePlain             // continuation lines fold with single spaces
	modeQuoted            // same-line-closed quoted scalar; later continuations fold leniently
	modeBlock             // indented lines collect and join per the indicator (| newline, > space)
)

// valueState is the pending value of the open recognized key.
type valueState struct {
	key  string
	val  string // scalar value so far; blocks finalize from lines
	mode int
	join string   // block join: "\n" for |, " " for >
	body []string // block content lines
}

// parseSkill parses one SKILL.md from subdirectory dir. A returned error
// means the skill is invalid — the caller skips it and records an Issue.
// Non-fatal findings (description truncation, a name that does not match the
// directory) come back as issues alongside a valid skill.
//
// The frontmatter is an agentskills.io-aligned YAML subset read by an
// indent-aware line scanner. Recognized keys (name, description) accept
// plain scalars with folded continuation lines, single-line quoted scalars
// with escapes, and block scalars (| and >). Any unknown key — together with
// its entire indented block, nested maps and lists included — is skipped and
// never enters the key space. Blank lines are tolerated and only close a
// recognized key's plain-family state. Hard errors are reserved for real
// authoring mistakes on the recognized keys: duplicate keys, a missing or
// empty description, unterminated quotes, flow collections, a missing fence,
// or an empty body.
func parseSkill(raw []byte, dir string) (Skill, []Issue, error) {
	skill := Skill{Dir: dir}
	if len(raw) > maxSkillFileBytes {
		return skill, nil, fmt.Errorf("SKILL.md exceeds the %d-byte cap", maxSkillFileBytes)
	}
	text := strings.ReplaceAll(strings.TrimPrefix(string(raw), "\uFEFF"), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if lines[0] != frontmatterFence {
		return skill, nil, errors.New("no frontmatter: file must start with a --- fence")
	}
	fields := make(map[string]string, 2)
	seen := make(map[string]bool, 2)
	var issues []Issue
	var st *valueState // the open recognized key, or nil when none is open
	skipping := false  // inside an unknown key's block; survives blank lines
	bodyFrom := 0
scan:
	for i := 1; i < len(lines); i++ {
		line := lines[i]
		switch {
		case strings.TrimSpace(line) == "":
			if st == nil {
				continue // skipping or nothing open: blank lines are inert
			}
			switch st.mode {
			case modeBlock:
				st.body = append(st.body, "")
			case modePlain, modeEmptyValue, modeQuoted:
				finalValue(fields, st) // flush the folded value before closing
				st = nil
			}
		case line != strings.TrimLeft(line, " \t"): // indented continuation
			trimmed := strings.TrimSpace(line)
			if skipping {
				continue
			}
			if st == nil {
				return skill, nil, fmt.Errorf("frontmatter line %d: expected a key: value line", i+1)
			}
			switch st.mode {
			case modeEmptyValue: // first content line is classified, never folded verbatim
				next, err := classifyScalar(trimmed, i+1)
				if err != nil {
					return skill, nil, err
				}
				st.val, st.mode, st.join = next.val, next.mode, next.join
				st.body = next.body
			case modePlain, modeQuoted:
				st.val = strings.TrimSpace(st.val + " " + trimmed)
			case modeBlock:
				st.body = append(st.body, trimmed)
			}
		case line == frontmatterFence: // closing fence; an indented "---" is not a fence
			finalValue(fields, st)
			bodyFrom = i + 1
			break scan
		default: // top-level line
			finalValue(fields, st)
			st = nil
			key, rest, found := strings.Cut(line, ":")
			key, rest = strings.TrimSpace(key), strings.TrimSpace(rest)
			if !found || key == "" {
				return skill, nil, fmt.Errorf("frontmatter line %d: expected a key: value line", i+1)
			}
			if key != "name" && key != "description" {
				// Unknown key: skip it and everything under it. Duplicates are
				// not tracked — a skipped field has no collision to detect.
				skipping = true
				continue
			}
			skipping = false
			if seen[key] {
				return skill, nil, fmt.Errorf("frontmatter line %d: duplicate key %q", i+1, key)
			}
			seen[key] = true
			next, err := classifyScalar(rest, i+1)
			if err != nil {
				return skill, nil, err
			}
			if next.mode == modeBlock {
				st = &valueState{key: key, mode: modeBlock, join: next.join, body: next.body}
			} else {
				fields[key] = next.val
				st = &valueState{key: key, val: next.val, mode: next.mode}
			}
		}
	}
	if bodyFrom == 0 {
		return skill, nil, errors.New("no frontmatter closing --- fence")
	}

	name := dir
	if explicit, ok := fields["name"]; ok {
		name = explicit
		if name != dir {
			issues = append(issues, Issue{Dir: dir, Skill: name,
				Problem: fmt.Sprintf("name %q does not match directory %q; layer-3 file references in the body resolve against the directory", name, dir)})
		}
	}
	skill.Name = name
	if !skillNamePattern.MatchString(name) || utf8.RuneCountInString(name) > maxNameRunes {
		return skill, nil, fmt.Errorf("invalid name %q: must match ^[a-z0-9]+(-[a-z0-9]+)*$ and be at most %d runes", name, maxNameRunes)
	}
	description, ok := fields["description"]
	if !ok {
		return skill, nil, errors.New("missing description")
	}
	skill.Description = strings.Join(strings.Fields(description), " ")
	if skill.Description == "" {
		return skill, nil, errors.New("empty description")
	}
	if utf8.RuneCountInString(skill.Description) > maxDescriptionRunes {
		skill.Description = string([]rune(skill.Description)[:maxDescriptionRunes]) + "…"
		issues = append(issues, Issue{Dir: dir, Skill: skill.Name,
			Problem: fmt.Sprintf("description truncated to %d runes", maxDescriptionRunes)})
	}
	skill.Body = strings.TrimSpace(strings.Join(lines[bodyFrom:], "\n"))
	if skill.Body == "" {
		return skill, nil, errors.New("empty body")
	}
	return skill, issues, nil
}

// finalizeValue writes the open state's final value into fields: a block
// joins its collected lines, a plain-family state flushes its folded value.
func finalValue(fields map[string]string, st *valueState) {
	if st == nil {
		return
	}
	if st.mode == modeBlock {
		fields[st.key] = strings.Join(st.body, st.join)
	} else {
		fields[st.key] = st.val
	}
}

// classifyScalar classifies one "key: rest" value (rest already trimmed) or
// one classified continuation line of an empty-value recognized key, per the
// aligned subset's scalar rules.
func classifyScalar(rest string, lineNo int) (valueState, error) {
	switch {
	case rest == "":
		return valueState{mode: modeEmptyValue}, nil
	case rest[0] == '"':
		val, err := unquoteDouble(rest)
		if err != nil {
			return valueState{}, fmt.Errorf("frontmatter line %d: %w", lineNo, err)
		}
		return valueState{val: val, mode: modeQuoted}, nil
	case rest[0] == '\'':
		val, err := unquoteSingle(rest)
		if err != nil {
			return valueState{}, fmt.Errorf("frontmatter line %d: %w", lineNo, err)
		}
		return valueState{val: val, mode: modeQuoted}, nil
	case rest[0] == '|' || rest[0] == '>':
		st := valueState{mode: modeBlock, join: "\n"}
		if rest[0] == '>' {
			st.join = " "
		}
		rest = strings.TrimLeft(rest[1:], "-+0123456789") // chomping/indent indicators
		if first := strings.TrimSpace(rest); first != "" {
			// Lenient extension: header trailing content becomes the block's
			// first line (strict YAML rejects this shape).
			st.body = append(st.body, first)
		}
		return st, nil
	case rest[0] == '[' || rest[0] == '{':
		return valueState{}, fmt.Errorf("frontmatter line %d: name/description must be a scalar", lineNo)
	default:
		return valueState{val: rest, mode: modePlain}, nil
	}
}

// unquoteDouble decodes one same-line double-quoted scalar: the closing
// unescaped quote must be on this line; \" \\ \n \t expand, other escapes
// keep both characters.
func unquoteDouble(s string) (string, error) {
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			if i+1 >= len(s) {
				return "", errors.New("unterminated double-quoted scalar")
			}
			i++
			switch s[i] {
			case '"':
				b.WriteByte('"')
			case '\\':
				b.WriteByte('\\')
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			default:
				b.WriteByte('\\')
				b.WriteByte(s[i])
			}
		case '"':
			return b.String(), nil
		default:
			b.WriteByte(s[i])
		}
	}
	return "", errors.New("unterminated double-quoted scalar")
}

// unquoteSingle decodes one same-line single-quoted scalar: ' is an escaped
// quote, the closing quote must be on this line.
func unquoteSingle(s string) (string, error) {
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		if s[i] != '\'' {
			b.WriteByte(s[i])
			continue
		}
		if i+1 < len(s) && s[i+1] == '\'' {
			b.WriteByte('\'')
			i++
			continue
		}
		return b.String(), nil
	}
	return "", errors.New("unterminated single-quoted scalar")
}
