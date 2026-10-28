// Package skills loads an Agent Skills directory into an immutable Set and
// exposes it as one distributor tool: the tool's Description carries the
// catalog (name + description per skill); Execute returns a named skill's
// body verbatim.
//
// Import github.com/dailz1/go-agent/skills. The package depends only on the
// tool seam and the standard library. Load scans one level of subdirectories;
// each skill is a subdirectory holding a SKILL.md whose frontmatter follows
// an agentskills.io-aligned YAML subset: recognized keys (name, description)
// accept plain scalars with folded continuations, same-line quoted scalars
// with escapes, and block scalars; unknown keys are skipped with their whole
// indented block. Invalid skills never fail Load — they are skipped and
// recorded in Set.Issues. Load fails only on configuration errors: an
// unreadable root, invalid option values, or an assembled catalog over its
// rune budget.
//
// A Set is immutable and safe for concurrent use. The binding contract is
// skills/README.md.
package skills

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/dailz1/go-agent/tool"
)

const (
	// defaultToolName is the distributor tool name unless WithToolName overrides it.
	defaultToolName = "skill"
	// defaultCatalogLimit is the default catalog budget in runes — roughly
	// 10–12 typical entries of name plus a ~120-rune description.
	defaultCatalogLimit = 2048
)

var toolNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Skill is one parsed skill.
type Skill struct {
	// Name is the effective name: the frontmatter name, or the
	// subdirectory name when the frontmatter omits name.
	Name string
	// Description is the whitespace-collapsed description, at most 1024
	// runes (longer inputs are truncated with an ellipsis and an Issue).
	Description string
	// Body is the verbatim markdown after the frontmatter, trimmed.
	Body string
	// Dir is the subdirectory name, for diagnostics and layer-3 file location.
	Dir string
}

// Issue records one skipped skill, or one finding on a loaded skill
// (name not matching its directory, description truncation), and why.
type Issue struct {
	Dir     string // subdirectory name; "" for root-level findings
	Skill   string // best-effort skill name; "" when none was resolvable
	Problem string
}

// Set is the immutable result of one Load. Its methods are safe for
// concurrent use.
type Set struct {
	skills   []Skill
	issues   []Issue
	byName   map[string]string
	catalog  string
	toolName string
}

// Load scans root's immediate subdirectories for SKILL.md files and returns
// the immutable skill set. See the package comment and skills/README.md for
// the scan, parse, and failure contract.
func Load(root string, opts ...Option) (*Set, error) {
	cfg, err := resolveConfig(opts)
	if err != nil {
		return nil, err
	}
	scanned, issues, err := scanRoot(root)
	if err != nil {
		return nil, err
	}
	set := &Set{toolName: cfg.toolName}
	owners := make(map[string]string, len(scanned))
	for _, entry := range scanned { // scan order is subdirectory sort order
		if winner, dup := owners[entry.skill.Name]; dup {
			issues = append(issues, Issue{Dir: entry.skill.Dir, Skill: entry.skill.Name,
				Problem: fmt.Sprintf("duplicate name shadowed by %q; this skill is skipped", winner)})
			continue
		}
		owners[entry.skill.Name] = entry.skill.Dir
		set.skills = append(set.skills, entry.skill)
		issues = append(issues, entry.issues...)
	}
	slices.SortFunc(set.skills, func(a, b Skill) int { return strings.Compare(a.Name, b.Name) })
	slices.SortStableFunc(issues, func(a, b Issue) int { return strings.Compare(a.Dir, b.Dir) })
	set.issues = issues
	set.catalog = assembleCatalog(set.skills)
	if n := utf8.RuneCountInString(set.catalog); n > cfg.catalogLimit {
		return nil, fmt.Errorf("skills catalog %d runes exceeds limit %d across %d skills: prune skills or raise the limit with WithCatalogLimit", n, cfg.catalogLimit, len(set.skills))
	}
	set.byName = make(map[string]string, len(set.skills))
	for _, s := range set.skills {
		set.byName[s.Name] = s.Body
	}
	return set, nil
}

// Tool returns the set's distributor tool; repeated calls return tools with
// identical Info. Registering it is the host's job.
func (s *Set) Tool() tool.Tool { return &distributor{set: s} }

// Skills returns a copy of the valid skills, sorted by Name.
func (s *Set) Skills() []Skill { return slices.Clone(s.skills) }

// Issues returns a copy of the recorded issues, sorted by Dir.
func (s *Set) Issues() []Issue { return slices.Clone(s.issues) }

type config struct {
	toolName     string
	catalogLimit int
}

// Option configures Load.
type Option func(*config)

// WithToolName sets the distributor tool name (default "skill"). The name
// must match ^[A-Za-z0-9_-]{1,64}$; invalid values fail Load.
func WithToolName(name string) Option {
	return func(c *config) { c.toolName = name }
}

// WithCatalogLimit sets the catalog's total rune budget (default 2048). It
// must be positive; an assembled catalog over budget fails Load.
func WithCatalogLimit(runes int) Option {
	return func(c *config) { c.catalogLimit = runes }
}

func resolveConfig(opts []Option) (config, error) {
	c := config{toolName: defaultToolName, catalogLimit: defaultCatalogLimit}
	for _, opt := range opts {
		if opt != nil {
			opt(&c)
		}
	}
	if !toolNamePattern.MatchString(c.toolName) {
		return config{}, fmt.Errorf("skills tool name %q must match ^[A-Za-z0-9_-]{1,64}$", c.toolName)
	}
	if c.catalogLimit <= 0 {
		return config{}, fmt.Errorf("skills catalog limit must be positive")
	}
	return c, nil
}
