package skills

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseSkill(t *testing.T) {
	tests := []struct {
		name       string
		dir        string
		raw        string
		want       Skill
		wantIssues []Issue
		wantErr    string
	}{
		{
			name: "minimal valid",
			dir:  "min-skill",
			raw:  "---\nname: min-skill\ndescription: Does one thing well.\n---\nUse it thus.",
			want: Skill{Name: "min-skill", Description: "Does one thing well.", Body: "Use it thus.", Dir: "min-skill"},
		},
		{
			name: "name defaults to the directory name",
			dir:  "derived-name",
			raw:  "---\ndescription: Desc here.\n---\nBody.",
			want: Skill{Name: "derived-name", Description: "Desc here.", Body: "Body.", Dir: "derived-name"},
		},
		{
			name:    "derived name must still be valid",
			dir:     "Bad_Dir",
			raw:     "---\ndescription: d\n---\nbody",
			wantErr: "invalid name",
		},
		{
			name:    "uppercase explicit name",
			dir:     "ok-dir",
			raw:     "---\nname: MinSkill\ndescription: d\n---\nbody",
			wantErr: "invalid name",
		},
		{
			name:    "underscore in name",
			dir:     "ok-dir",
			raw:     "---\nname: min_skill\ndescription: d\n---\nbody",
			wantErr: "invalid name",
		},
		{
			name:    "leading hyphen",
			dir:     "ok-dir",
			raw:     "---\nname: -abc\ndescription: d\n---\nbody",
			wantErr: "invalid name",
		},
		{
			name:    "trailing hyphen",
			dir:     "ok-dir",
			raw:     "---\nname: abc-\ndescription: d\n---\nbody",
			wantErr: "invalid name",
		},
		{
			name:    "name over 64 runes",
			dir:     "ok-dir",
			raw:     "---\nname: " + strings.Repeat("a", 65) + "\ndescription: d\n---\nbody",
			wantErr: "invalid name",
		},
		{
			name: "name of exactly 64 runes",
			dir:  strings.Repeat("a", 64),
			raw:  "---\nname: " + strings.Repeat("a", 64) + "\ndescription: d\n---\nbody",
			want: Skill{Name: strings.Repeat("a", 64), Description: "d", Body: "body", Dir: strings.Repeat("a", 64)},
		},
		{
			name: "single-quoted name",
			dir:  "quoted-name",
			raw:  "---\nname: 'quoted-name'\ndescription: d\n---\nbody",
			want: Skill{Name: "quoted-name", Description: "d", Body: "body", Dir: "quoted-name"},
		},
		{
			name: "double-quoted description",
			dir:  "q",
			raw:  "---\nname: q\ndescription: \"Quoted desc.\"\n---\nbody",
			want: Skill{Name: "q", Description: "Quoted desc.", Body: "body", Dir: "q"},
		},
		{
			name:    "description missing",
			dir:     "ok-dir",
			raw:     "---\nname: q\n---\nbody",
			wantErr: "missing description",
		},
		{
			name:    "description empty value",
			dir:     "ok-dir",
			raw:     "---\nname: q\ndescription:\n---\nbody",
			wantErr: "empty description",
		},
		{
			name:    "description whitespace only",
			dir:     "ok-dir",
			raw:     "---\nname: q\ndescription: \t  \n---\nbody",
			wantErr: "empty description",
		},
		{
			name: "description whitespace collapses to single spaces",
			dir:  "fold",
			raw:  "---\nname: fold\ndescription: one\t two   three\rfour\n---\nB.",
			want: Skill{Name: "fold", Description: "one two three four", Body: "B.", Dir: "fold"},
		},
		{
			name: "value keeps inner colons",
			dir:  "colon",
			raw:  "---\nname: colon\ndescription: Time: now\n---\nbody",
			want: Skill{Name: "colon", Description: "Time: now", Body: "body", Dir: "colon"},
		},
		{
			name: "description of exactly 1024 runes",
			dir:  "cap",
			raw:  "---\nname: cap\ndescription: " + strings.Repeat("x", 1024) + "\n---\nbody",
			want: Skill{Name: "cap", Description: strings.Repeat("x", 1024), Body: "body", Dir: "cap"},
		},
		{
			name:       "description of 1025 runes truncates with an issue",
			dir:        "cap",
			raw:        "---\nname: cap\ndescription: " + strings.Repeat("x", 1025) + "\n---\nbody",
			want:       Skill{Name: "cap", Description: strings.Repeat("x", 1024) + "…", Body: "body", Dir: "cap"},
			wantIssues: []Issue{{Dir: "cap", Skill: "cap", Problem: "description truncated to 1024 runes"}},
		},
		{
			name:       "multibyte description truncates by rune",
			dir:        "cap",
			raw:        "---\nname: cap\ndescription: " + strings.Repeat("é", 1025) + "\n---\nbody",
			want:       Skill{Name: "cap", Description: strings.Repeat("é", 1024) + "…", Body: "body", Dir: "cap"},
			wantIssues: []Issue{{Dir: "cap", Skill: "cap", Problem: "description truncated to 1024 runes"}},
		},
		{
			name:    "duplicate recognized key",
			dir:     "ok-dir",
			raw:     "---\nname: q\nname: r\ndescription: d\n---\nbody",
			wantErr: "duplicate key",
		},
		{
			name: "duplicate unknown key is ignored",
			dir:  "q",
			raw:  "---\nextra: 1\nextra: 2\nname: q\ndescription: d\n---\nbody",
			want: Skill{Name: "q", Description: "d", Body: "body", Dir: "q"},
		},
		{
			name: "block scalar on description folds its lines",
			dir:  "q",
			raw:  "---\nname: q\ndescription: |\n  folded text\n---\nbody",
			want: Skill{Name: "q", Description: "folded text", Body: "body", Dir: "q"},
		},
		{
			name: "folded scalar header trailing content is the first block line",
			dir:  "q",
			raw:  "---\nname: q\ndescription: >- folded\n---\nbody",
			want: Skill{Name: "q", Description: "folded", Body: "body", Dir: "q"},
		},
		{
			name:    "block scalar on name fails the name regex",
			dir:     "ok-dir",
			raw:     "---\nname: |\n  two words\ndescription: d\n---\nbody",
			wantErr: "invalid name",
		},
		{
			name: "plain multiline continuation folds",
			dir:  "q",
			raw:  "---\nname: q\ndescription: first part\n  second part\n---\nbody",
			want: Skill{Name: "q", Description: "first part second part", Body: "body", Dir: "q"},
		},
		{
			name: "empty value waits for indented content",
			dir:  "q",
			raw:  "---\nname: q\ndescription:\n  written across lines\n---\nbody",
			want: Skill{Name: "q", Description: "written across lines", Body: "body", Dir: "q"},
		},
		{
			name: "empty value first continuation is classified as quoted",
			dir:  "q",
			raw:  "---\nname: q\ndescription:\n  \"Use \\\"pdfcpu\\\" here\"\n---\nbody",
			want: Skill{Name: "q", Description: `Use "pdfcpu" here`, Body: "body", Dir: "q"},
		},
		{
			name:    "empty value first continuation flow collection is a hard error",
			dir:     "ok-dir",
			raw:     "---\nname: q\ndescription:\n  [a, b]\n---\nbody",
			wantErr: "must be a scalar",
		},
		{
			name: "unknown list block is skipped",
			dir:  "q",
			raw:  "---\nname: q\nallowed-tools:\n  - Bash\n  - Edit\ndescription: d\n---\nbody",
			want: Skill{Name: "q", Description: "d", Body: "body", Dir: "q"},
		},
		{
			name: "nested metadata subkey shadowing a recognized key is skipped",
			dir:  "m",
			raw:  "---\nmetadata:\n  name: inner\nname: m\ndescription: d\n---\nbody",
			want: Skill{Name: "m", Description: "d", Body: "body", Dir: "m"},
		},
		{
			name: "unknown block scalar is skipped",
			dir:  "q",
			raw:  "---\nname: q\nextra: |\n  anything\n  goes\ndescription: d\n---\nbody",
			want: Skill{Name: "q", Description: "d", Body: "body", Dir: "q"},
		},
		{
			name: "blank line inside an unknown block keeps skipping",
			dir:  "q",
			raw:  "---\nmetadata:\n  author: Alice\n\n  version: \"1.0\"\nname: q\ndescription: d\n---\nbody",
			want: Skill{Name: "q", Description: "d", Body: "body", Dir: "q"},
		},
		{
			name: "blank line inside a block scalar is content",
			dir:  "q",
			raw:  "---\nname: q\ndescription: |\n  line one\n\n  line two\n---\nbody",
			want: Skill{Name: "q", Description: "line one line two", Body: "body", Dir: "q"},
		},
		{
			name: "double-quoted escapes expand",
			dir:  "q",
			raw:  "---\nname: q\ndescription: \"Use \\\"pdfcpu\\\" and C:\\\\path\"\n---\nbody",
			want: Skill{Name: "q", Description: `Use "pdfcpu" and C:\path`, Body: "body", Dir: "q"},
		},
		{
			name: "unknown double-quoted escape keeps both characters",
			dir:  "q",
			raw:  "---\nname: q\ndescription: \"a \\x b\"\n---\nbody",
			want: Skill{Name: "q", Description: `a \x b`, Body: "body", Dir: "q"},
		},
		{
			name: "single-quoted escaped quote collapses",
			dir:  "q",
			raw:  "---\nname: q\ndescription: 'It''s here'\n---\nbody",
			want: Skill{Name: "q", Description: "It's here", Body: "body", Dir: "q"},
		},
		{
			name:    "unterminated double quote",
			dir:     "ok-dir",
			raw:     "---\nname: q\ndescription: \"oops\n---\nbody",
			wantErr: "unterminated double-quoted scalar",
		},
		{
			name:    "unterminated single quote",
			dir:     "ok-dir",
			raw:     "---\nname: q\ndescription: 'oops\n---\nbody",
			wantErr: "unterminated single-quoted scalar",
		},
		{
			name:       "name mismatching the directory loads with an issue",
			dir:        "other-dir",
			raw:        "---\nname: zzz\ndescription: d\n---\nbody",
			want:       Skill{Name: "zzz", Description: "d", Body: "body", Dir: "other-dir"},
			wantIssues: []Issue{{Dir: "other-dir", Skill: "zzz", Problem: `name "zzz" does not match directory "other-dir"; layer-3 file references in the body resolve against the directory`}},
		},
		{
			name:    "flow collection on description",
			dir:     "ok-dir",
			raw:     "---\nname: q\ndescription: [a, b]\n---\nbody",
			wantErr: "must be a scalar",
		},
		{
			name:    "indented colon-less line before any key",
			dir:     "ok-dir",
			raw:     "---\n  just text\nname: q\ndescription: d\n---\nbody",
			wantErr: "expected a key: value line",
		},
		{
			name: "continuation after a quoted scalar folds leniently",
			dir:  "q",
			raw:  "---\nname: q\ndescription: \"quoted\"\n  more\n---\nbody",
			want: Skill{Name: "q", Description: "quoted more", Body: "body", Dir: "q"},
		},
		{
			name:    "line without a colon",
			dir:     "ok-dir",
			raw:     "---\nname: q\ndescription: d\njust text\n---\nbody",
			wantErr: "expected a key: value line",
		},
		{
			name: "blank line between keys keeps the folded value",
			dir:  "q",
			raw:  "---\nname: q\ndescription: first part\n  second part\n\nlicense: MIT\n---\nbody",
			want: Skill{Name: "q", Description: "first part second part", Body: "body", Dir: "q"},
		},
		{
			name:    "empty key",
			dir:     "ok-dir",
			raw:     "---\n: value\nname: q\ndescription: d\n---\nbody",
			wantErr: "expected a key: value line",
		},
		{
			name:    "missing closing fence",
			dir:     "ok-dir",
			raw:     "---\nname: q\ndescription: d",
			wantErr: "closing",
		},
		{
			name:    "no frontmatter",
			dir:     "ok-dir",
			raw:     "# Title\n\nBody.",
			wantErr: "must start with",
		},
		{
			name: "BOM is stripped",
			dir:  "bom",
			raw:  "\uFEFF---\nname: bom\ndescription: b\n---\nBody.",
			want: Skill{Name: "bom", Description: "b", Body: "Body.", Dir: "bom"},
		},
		{
			name: "CRLF is normalized",
			dir:  "crlf",
			raw:  "---\r\nname: crlf\r\ndescription: c\r\n---\r\nline one\r\nline two",
			want: Skill{Name: "crlf", Description: "c", Body: "line one\nline two", Dir: "crlf"},
		},
		{
			name:    "empty body",
			dir:     "ok-dir",
			raw:     "---\nname: e\ndescription: d\n---\n",
			wantErr: "empty body",
		},
		{
			name:    "whitespace-only body",
			dir:     "ok-dir",
			raw:     "---\nname: e\ndescription: d\n---\n \n\t\n",
			wantErr: "empty body",
		},
		{
			name: "body edges trimmed, interior preserved",
			dir:  "b",
			raw:  "---\nname: b\ndescription: d\n---\n\nfirst\n\nlast\n\n",
			want: Skill{Name: "b", Description: "d", Body: "first\n\nlast", Dir: "b"},
		},
		{
			name:    "file over the 1 MiB cap",
			dir:     "ok-dir",
			raw:     "---\n" + strings.Repeat("a", maxSkillFileBytes),
			wantErr: "exceeds the",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			skill, issues, err := parseSkill([]byte(tt.raw), tt.dir)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("parseSkill() error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseSkill() error = %v", err)
			}
			if !reflect.DeepEqual(skill, tt.want) {
				t.Errorf("parseSkill() skill = %#v, want %#v", skill, tt.want)
			}
			if !reflect.DeepEqual(issues, tt.wantIssues) {
				t.Errorf("parseSkill() issues = %#v, want %#v", issues, tt.wantIssues)
			}
		})
	}
}
