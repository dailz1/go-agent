package skills

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

func writeSkillDir(t *testing.T, dir, skillMD string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, dir, "SKILL.md"), []byte(skillMD), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

const wantBasicCatalog = "Return the full instructions of a skill by name. Available skills:\n" +
	"<skill><name>git-release</name><description>Guide for cutting a tagged release, from changelog to push.</description></skill>\n" +
	"<skill><name>pdf-forms</name><description>Fill and flatten PDF form fields with the pdfcpu CLI.</description></skill>"

func TestCatalogGolden(t *testing.T) {
	set, err := Load("testdata/basic")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := set.Tool().Info().Description; got != wantBasicCatalog {
		t.Errorf("catalog =\n%s\nwant\n%s", got, wantBasicCatalog)
	}
}

func TestCatalogSortedByNameOverMixedCaseDirs(t *testing.T) {
	set, err := Load("testdata/mixed-entries")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := catalogHeader + "\n" +
		"<skill><name>aaa</name><description>First by name though its directory sorts last.</description></skill>\n" +
		"<skill><name>zzz</name><description>Last by name though its directory sorts first.</description></skill>"
	if got := set.Tool().Info().Description; got != want {
		t.Errorf("catalog =\n%s\nwant\n%s", got, want)
	}
}

func TestCatalogEmptySetIsHeaderOnly(t *testing.T) {
	set, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := set.Tool().Info().Description; got != catalogHeader {
		t.Errorf("empty-set catalog = %q, want the bare header", got)
	}
}

func TestCatalogLimitBoundary(t *testing.T) {
	root := writeSkillDir(t, "one", "---\nname: one\ndescription: d\n---\nbody")
	set, err := Load(root)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	n := utf8.RuneCountInString(set.Tool().Info().Description)
	if _, err := Load(root, WithCatalogLimit(n)); err != nil {
		t.Fatalf("Load(limit = exact %d) error = %v", n, err)
	}
	_, err = Load(root, WithCatalogLimit(n-1))
	if err == nil {
		t.Fatalf("Load(limit = %d) succeeded, want overflow error", n-1)
	}
	for _, want := range []string{strconv.Itoa(n), strconv.Itoa(n - 1), "1 skills", "prune"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

func TestTruncatedDescriptionFeedsTheCatalog(t *testing.T) {
	root := writeSkillDir(t, "long", "---\nname: long\ndescription: "+strings.Repeat("x", 1100)+"\n---\nbody")
	set, err := Load(root)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if issues := set.Issues(); len(issues) != 1 || issues[0].Skill != "long" {
		t.Fatalf("Issues() = %#v, want one truncation for long", issues)
	}
	wantEntry := "<description>" + strings.Repeat("x", 1024) + "…</description>"
	if got := set.Tool().Info().Description; !strings.Contains(got, wantEntry) {
		t.Errorf("catalog missing truncated entry %q", wantEntry)
	}
	if skills := set.Skills(); !reflect.DeepEqual([]string{skills[0].Description}, []string{strings.Repeat("x", 1024) + "…"}) {
		t.Errorf("Skills()[0].Description length = %d runes", utf8.RuneCountInString(skills[0].Description))
	}
}
