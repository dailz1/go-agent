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
			dir:  "min",
			raw:  "---\nname: min-skill\ndescription: Does one thing well.\n---\nUse it thus.",
			want: Skill{Name: "min-skill", Description: "Does one thing well.", Body: "Use it thus.", Dir: "min"},
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
			dir:  "ok-dir",
			raw:  "---\nname: " + strings.Repeat("a", 64) + "\ndescription: d\n---\nbody",
			want: Skill{Name: strings.Repeat("a", 64), Description: "d", Body: "body", Dir: "ok-dir"},
		},
		{
			name: "single-quoted name",
			dir:  "ok-dir",
			raw:  "---\nname: 'quoted-name'\ndescription: d\n---\nbody",
			want: Skill{Name: "quoted-name", Description: "d", Body: "body", Dir: "ok-dir"},
		},
		{
			name: "double-quoted description",
			dir:  "ok-dir",
			raw:  "---\nname: q\ndescription: \"Quoted desc.\"\n---\nbody",
			want: Skill{Name: "q", Description: "Quoted desc.", Body: "body", Dir: "ok-dir"},
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
			dir:  "ok-dir",
			raw:  "---\nname: fold\ndescription: one\t two   three\rfour\n---\nB.",
			want: Skill{Name: "fold", Description: "one two three four", Body: "B.", Dir: "ok-dir"},
		},
		{
			name: "value keeps inner colons",
			dir:  "ok-dir",
			raw:  "---\nname: colon\ndescription: Time: now\n---\nbody",
			want: Skill{Name: "colon", Description: "Time: now", Body: "body", Dir: "ok-dir"},
		},
		{
			name: "description of exactly 1024 runes",
			dir:  "ok-dir",
			raw:  "---\nname: cap\ndescription: " + strings.Repeat("x", 1024) + "\n---\nbody",
			want: Skill{Name: "cap", Description: strings.Repeat("x", 1024), Body: "body", Dir: "ok-dir"},
		},
		{
			name:       "description of 1025 runes truncates with an issue",
			dir:        "ok-dir",
			raw:        "---\nname: cap\ndescription: " + strings.Repeat("x", 1025) + "\n---\nbody",
			want:       Skill{Name: "cap", Description: strings.Repeat("x", 1024) + "…", Body: "body", Dir: "ok-dir"},
			wantIssues: []Issue{{Dir: "ok-dir", Skill: "cap", Problem: "description truncated to 1024 runes"}},
		},
		{
			name:       "multibyte description truncates by rune",
			dir:        "ok-dir",
			raw:        "---\nname: cap\ndescription: " + strings.Repeat("é", 1025) + "\n---\nbody",
			want:       Skill{Name: "cap", Description: strings.Repeat("é", 1024) + "…", Body: "body", Dir: "ok-dir"},
			wantIssues: []Issue{{Dir: "ok-dir", Skill: "cap", Problem: "description truncated to 1024 runes"}},
		},
		{
			name:    "duplicate recognized key",
			dir:     "ok-dir",
			raw:     "---\nname: q\nname: r\ndescription: d\n---\nbody",
			wantErr: "duplicate key",
		},
		{
			name:    "duplicate unknown key",
			dir:     "ok-dir",
			raw:     "---\nextra: 1\nextra: 2\nname: q\ndescription: d\n---\nbody",
			wantErr: "duplicate key",
		},
		{
			name:    "block scalar",
			dir:     "ok-dir",
			raw:     "---\nname: q\ndescription: |\n  folded text\n---\nbody",
			wantErr: "not supported",
		},
		{
			name:    "folded scalar",
			dir:     "ok-dir",
			raw:     "---\nname: q\ndescription: >- folded\n---\nbody",
			wantErr: "not supported",
		},
		{
			name:    "line without a colon",
			dir:     "ok-dir",
			raw:     "---\nname: q\ndescription: d\njust text\n---\nbody",
			wantErr: "expected a key: value line",
		},
		{
			name:    "blank line inside frontmatter",
			dir:     "ok-dir",
			raw:     "---\nname: q\n\ndescription: d\n---\nbody",
			wantErr: "expected a key: value line",
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
			dir:  "ok-dir",
			raw:  "\uFEFF---\nname: bom\ndescription: b\n---\nBody.",
			want: Skill{Name: "bom", Description: "b", Body: "Body.", Dir: "ok-dir"},
		},
		{
			name: "CRLF is normalized",
			dir:  "ok-dir",
			raw:  "---\r\nname: crlf\r\ndescription: c\r\n---\r\nline one\r\nline two",
			want: Skill{Name: "crlf", Description: "c", Body: "line one\nline two", Dir: "ok-dir"},
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
			dir:  "ok-dir",
			raw:  "---\nname: b\ndescription: d\n---\n\nfirst\n\nlast\n\n",
			want: Skill{Name: "b", Description: "d", Body: "first\n\nlast", Dir: "ok-dir"},
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
