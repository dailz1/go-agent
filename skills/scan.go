package skills

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const (
	// skillFileName is the case-sensitive skill manifest basename.
	skillFileName = "SKILL.md"
	// maxSkillFileBytes caps one SKILL.md read — bounded I/O, same 1 MiB
	// family as the MCP result envelope.
	maxSkillFileBytes = 1 << 20
)

// scannedSkill is one subdirectory's parse outcome: a valid skill plus any
// non-fatal issues gathered along the way (description truncation).
type scannedSkill struct {
	skill  Skill
	issues []Issue
}

// scanRoot reads root's immediate subdirectories in sorted order. A returned
// error means Load fails: only an unreadable root qualifies. Non-directory
// entries are ignored; a root-level SKILL.md is reported as an issue.
func scanRoot(root string) ([]scannedSkill, []Issue, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, nil, fmt.Errorf("read skills root: %w", err)
	}
	var skills []scannedSkill
	var issues []Issue
	for _, entry := range entries { // os.ReadDir sorts by name
		if !entry.IsDir() {
			if entry.Name() == skillFileName {
				issues = append(issues, Issue{Problem: "root-level SKILL.md is not a skill: skills live in subdirectories"})
			}
			continue
		}
		skill, dirIssues, err := loadSkillDir(filepath.Join(root, entry.Name()))
		if err != nil {
			issues = append(issues, Issue{Dir: entry.Name(), Skill: skill.Name, Problem: err.Error()})
			continue
		}
		skills = append(skills, scannedSkill{skill: skill, issues: dirIssues})
	}
	return skills, issues, nil
}

// loadSkillDir reads and parses one skill subdirectory's SKILL.md. On error
// the returned Skill carries whatever was resolvable for diagnostics.
func loadSkillDir(dir string) (Skill, []Issue, error) {
	name := filepath.Base(dir)
	raw, err := readSkillFile(filepath.Join(dir, skillFileName))
	if err != nil {
		return Skill{Dir: name}, nil, err
	}
	return parseSkill(raw, name)
}

// readSkillFile reads at most maxSkillFileBytes+1 bytes so an over-cap file
// is detectable without unbounded I/O.
func readSkillFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errors.New("missing SKILL.md")
		}
		return nil, fmt.Errorf("open SKILL.md: %w", err)
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxSkillFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read SKILL.md: %w", err)
	}
	return raw, nil
}
