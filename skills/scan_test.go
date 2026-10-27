package skills

import (
	"reflect"
	"strings"
	"testing"
)

func TestLoadScanRoots(t *testing.T) {
	t.Run("missing root is a hard error", func(t *testing.T) {
		_, err := Load("testdata/does-not-exist")
		if err == nil || !strings.Contains(err.Error(), "read skills root") {
			t.Fatalf("Load() error = %v, want a read-skills-root error", err)
		}
	})
	t.Run("root is a file is a hard error", func(t *testing.T) {
		_, err := Load("testdata/basic/git-release/SKILL.md")
		if err == nil || !strings.Contains(err.Error(), "read skills root") {
			t.Fatalf("Load() error = %v, want a read-skills-root error", err)
		}
	})
	t.Run("empty root yields an empty set", func(t *testing.T) {
		set, err := Load(t.TempDir())
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if len(set.Skills()) != 0 || len(set.Issues()) != 0 {
			t.Fatalf("Skills()/Issues() = %#v, %#v; want empty", set.Skills(), set.Issues())
		}
	})
	t.Run("subdirectory without SKILL.md is skipped and reported", func(t *testing.T) {
		set, err := Load("testdata/missing-file")
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		skills := set.Skills()
		if len(skills) != 1 || skills[0].Name != "happy" {
			t.Fatalf("Skills() = %#v, want only happy", skills)
		}
		issues := set.Issues()
		if len(issues) != 1 || issues[0].Dir != "lonely" || issues[0].Problem != "missing SKILL.md" {
			t.Fatalf("Issues() = %#v, want lonely/missing SKILL.md", issues)
		}
	})
	t.Run("non-directory entries are silently ignored", func(t *testing.T) {
		set, err := Load("testdata/mixed-entries")
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if len(set.Issues()) != 0 {
			t.Fatalf("Issues() = %#v, want none for a plain root file", set.Issues())
		}
		skills := set.Skills()
		if len(skills) != 2 {
			t.Fatalf("Skills() = %#v, want aaa and zzz", skills)
		}
		if skills[0].Name != "aaa" || skills[0].Dir != "gamma-dir" || skills[1].Name != "zzz" || skills[1].Dir != "Alpha-dir" {
			t.Fatalf("Skills() = %#v, want name-sorted aaa(gamma-dir) then zzz(Alpha-dir)", skills)
		}
	})
	t.Run("root-level SKILL.md is not a skill", func(t *testing.T) {
		set, err := Load("testdata/root-skillmd")
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		skills := set.Skills()
		if len(skills) != 1 || skills[0].Name != "one" {
			t.Fatalf("Skills() = %#v, want only one", skills)
		}
		issues := set.Issues()
		if len(issues) != 1 || issues[0].Dir != "" || !strings.Contains(issues[0].Problem, "root-level SKILL.md is not a skill") {
			t.Fatalf("Issues() = %#v, want a root-level finding", issues)
		}
	})
	t.Run("scanning does not recurse", func(t *testing.T) {
		set, err := Load("testdata/nested")
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		skills := set.Skills()
		if len(skills) != 1 || skills[0].Name != "outer" {
			t.Fatalf("Skills() = %#v, want only outer; inner must not appear", skills)
		}
	})
}

func TestLoadDuplicateNames(t *testing.T) {
	set, err := Load("testdata/collision")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	skills := set.Skills()
	if len(skills) != 2 {
		t.Fatalf("Skills() = %#v, want gamma and shared-name only", skills)
	}
	want := []Skill{
		{Name: "gamma", Description: "A unique name untouched by the collision.", Body: "Body from gamma.", Dir: "gamma"},
		{Name: "shared-name", Description: "Alpha flavor of the shared name.", Body: "Body from alpha.", Dir: "alpha"},
	}
	if !reflect.DeepEqual(skills, want) {
		t.Errorf("Skills() = %#v, want %#v", skills, want)
	}
	issues := set.Issues()
	if len(issues) != 1 || issues[0].Dir != "beta" || issues[0].Skill != "shared-name" ||
		!strings.Contains(issues[0].Problem, `shadowed by "alpha"`) {
		t.Errorf("Issues() = %#v, want beta shadowed by alpha", issues)
	}
}
