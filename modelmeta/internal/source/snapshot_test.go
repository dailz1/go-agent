package source

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func fixtureSnapshot() *Snapshot {
	at := time.Date(2026, 11, 20, 1, 2, 3, 0, time.UTC)
	return &Snapshot{
		SchemaVersion: 2, VerifiedAt: at,
		Sources: []Info{
			{ID: V1ID, URL: V1URL, SHA256: strings.Repeat("a", 64), VerifiedAt: at},
			{ID: V2ID, URL: V2URL, SHA256: strings.Repeat("b", 64), VerifiedAt: at.Add(time.Second)},
		},
		Windows: map[string]map[string]int{Codex: {"z": 20, "a": 10}, "p": {"b": 30}},
	}
}

func TestSnapshotRejectsIncompleteOrCorruptFormat(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Snapshot)
	}{
		{"old format", func(s *Snapshot) { s.SchemaVersion = 1 }},
		{"missing source", func(s *Snapshot) { s.Sources = s.Sources[:1] }},
		{"duplicate source", func(s *Snapshot) { s.Sources[1] = s.Sources[0] }},
		{"wrong url", func(s *Snapshot) { s.Sources[0].URL = V2URL }},
		{"zero time", func(s *Snapshot) { s.Sources[0].VerifiedAt = time.Time{} }},
		{"wrong age", func(s *Snapshot) { s.VerifiedAt = s.VerifiedAt.Add(time.Second) }},
		{"bad digest", func(s *Snapshot) { s.Sources[0].SHA256 = "no" }},
		{"short digest", func(s *Snapshot) { s.Sources[0].SHA256 = "aa" }},
		{"negative skip", func(s *Snapshot) { s.Sources[0].SkippedModels = -1 }},
		{"missing codex", func(s *Snapshot) { delete(s.Windows, Codex) }},
		{"zero window", func(s *Snapshot) { s.Windows[Codex]["z"] = 0 }},
		{"oversized window", func(s *Snapshot) { s.Windows[Codex]["z"] = 2147483648 }},
		{"null provider", func(s *Snapshot) { s.Windows["p"] = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := fixtureSnapshot()
			tc.change(s)
			data, err := json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			if got, err := ParseSnapshot(data); err == nil || got != nil {
				t.Fatalf("parse = %v, %v", got, err)
			}
		})
	}
}

func TestCanonicalSortsEveryObjectAndRetainsExactNumbers(t *testing.T) {
	s := fixtureSnapshot()
	got, err := Canonical(s)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema_version":2,"sources":[{"etag":"","id":"models-dev-v1","sha256":"` + strings.Repeat("a", 64) +
		`","skipped_models":0,"url":"https://models.dev/api.json","verified_at":"2026-11-20T01:02:03Z"},{"etag":"","id":"models-dev-v2-codex","sha256":"` +
		strings.Repeat("b", 64) + `","skipped_models":0,"url":"https://models.dev/experimental/v2.0/api.json?include=openai-codex&type=chat","verified_at":"2026-11-20T01:02:04Z"}],"verified_at":"2026-11-20T01:02:03Z","windows":{"openai-codex":{"a":10,"z":20},"p":{"b":30}}}` + "\n"
	if string(got) != want {
		t.Fatalf("canonical = %s; want %s", got, want)
	}
	s.Sources[0], s.Sources[1] = s.Sources[1], s.Sources[0]
	again, err := Canonical(s)
	if err != nil || !bytes.Equal(got, again) {
		t.Fatalf("canonical unstable: %v", err)
	}
	if s.Sources[0].ID != V2ID {
		t.Fatal("canonical mutated source slice")
	}
}

func TestPreparedSnapshotRoundTrip(t *testing.T) {
	data, err := os.ReadFile("../../data/windows.json")
	if err != nil {
		t.Fatal(err)
	}
	s, err := ParseSnapshot(data)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := Canonical(s)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseSnapshot(canonical)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := Canonical(parsed)
	if err != nil || !bytes.Equal(canonical, repeated) {
		t.Fatalf("canonical round trip changed bytes: %v", err)
	}
}
