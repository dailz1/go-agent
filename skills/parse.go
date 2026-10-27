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

// parseSkill parses one SKILL.md from subdirectory dir. A returned error
// means the skill is invalid — the caller skips it and records an Issue.
// Non-fatal findings (description truncation) come back as issues alongside
// a valid skill. The subset is flat "key: value" lines between --- fences:
// recognized keys are name and description, unknown keys are ignored, and
// anything else — duplicate keys, block/folded scalars, malformed lines, a
// missing fence, an empty description or body — invalidates the skill.
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
	bodyFrom := 0
	for i := 1; i < len(lines); i++ {
		if lines[i] == frontmatterFence {
			bodyFrom = i + 1
			break
		}
		key, value, err := parseFrontmatterLine(lines[i])
		if err != nil {
			return skill, nil, fmt.Errorf("frontmatter line %d: %w", i+1, err)
		}
		if _, dup := fields[key]; dup {
			return skill, nil, fmt.Errorf("frontmatter line %d: duplicate key %q", i+1, key)
		}
		fields[key] = value
	}
	if bodyFrom == 0 {
		return skill, nil, errors.New("no frontmatter closing --- fence")
	}
	name := dir
	if explicit, ok := fields["name"]; ok {
		name = explicit
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
	var issues []Issue
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

// parseFrontmatterLine parses one flat "key: value" line.
func parseFrontmatterLine(line string) (key, value string, err error) {
	key, value, found := strings.Cut(line, ":")
	if !found {
		return "", "", errors.New("expected a key: value line")
	}
	key, value = strings.TrimSpace(key), strings.TrimSpace(value)
	if key == "" {
		return "", "", errors.New("expected a key: value line")
	}
	if strings.HasPrefix(value, "|") || strings.HasPrefix(value, ">") {
		return "", "", errors.New("block and folded scalars (| and >) are not supported")
	}
	return key, unquote(value), nil
}

// unquote strips one pair of matching surrounding quotes.
func unquote(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		return v[1 : len(v)-1]
	}
	return v
}
