package skills

import (
	"reflect"
	"strings"
	"testing"
)

func TestLoadOptionValidation(t *testing.T) {
	tests := []struct {
		name    string
		opt     Option
		wantErr string
	}{
		{"tool name with a space", WithToolName("bad name"), "must match"},
		{"empty tool name", WithToolName(""), "must match"},
		{"tool name over 64 chars", WithToolName(strings.Repeat("a", 65)), "must match"},
		{"zero catalog limit", WithCatalogLimit(0), "must be positive"},
		{"negative catalog limit", WithCatalogLimit(-1), "must be positive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load("testdata/basic", tt.opt)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Load() error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestWithToolNameRenamesDistributor(t *testing.T) {
	set, err := Load("testdata/basic", WithToolName("Skill_1-2"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := set.Tool().Info().Name; got != "Skill_1-2" {
		t.Errorf("Info().Name = %q, want Skill_1-2", got)
	}
}

func TestLoadDefaultsEndToEnd(t *testing.T) {
	set, err := Load("testdata/defaults")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := []Skill{{
		Name:        "derived-name",
		Description: "Defaults and unknown keys.",
		Body:        "Body of the derived skill.",
		Dir:         "derived-name",
	}}
	if got := set.Skills(); !reflect.DeepEqual(got, want) {
		t.Errorf("Skills() = %#v, want %#v", got, want)
	}
	if issues := set.Issues(); len(issues) != 0 {
		t.Errorf("Issues() = %#v, want none", issues)
	}
}
