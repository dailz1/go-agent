package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestManagerCreatePersistsMetaBeforeExecution(t *testing.T) {
	dir := t.TempDir()
	m, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	meta, err := m.Create("/work/project", "openai", "gpt-test")
	if err != nil {
		t.Fatal(err)
	}
	if meta.ID == "" || meta.LastKnown != StatusCreated {
		t.Fatalf("created meta = %+v", meta)
	}

	// The identity is durable the moment Create returns: a second manager
	// (simulating a restart before any model call) must list it.
	data, err := os.ReadFile(filepath.Join(dir, "sessions", meta.ID, "meta.json"))
	if err != nil {
		t.Fatalf("meta not persisted before execution: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("empty meta file")
	}
	info, err := os.Stat(filepath.Join(dir, "sessions", meta.ID))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("session dir mode = %v, want 0700", info.Mode().Perm())
	}
	minfo, err := os.Stat(filepath.Join(dir, "sessions", meta.ID, "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	if minfo.Mode().Perm() != 0o600 {
		t.Fatalf("meta mode = %v, want 0600", minfo.Mode().Perm())
	}
}

func TestManagerListOrdersByUpdate(t *testing.T) {
	now := time.Unix(1700000000, 0)
	dir := t.TempDir()
	m, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	m.now = func() time.Time { return now }

	first, err := m.Create("/w", "openai", "m")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	second, err := m.Create("/w", "openai", "m")
	if err != nil {
		t.Fatal(err)
	}

	list, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != second.ID || list[1].ID != first.ID {
		t.Fatalf("list order = [%s %s], want newest first", list[0].ID, list[1].ID)
	}
}

func TestManagerUpdateRoundTrips(t *testing.T) {
	m, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	meta, err := m.Create("/w", "glm", "glm-4")
	if err != nil {
		t.Fatal(err)
	}
	meta.Title = Title("  fix the parser test\n\nsecond line")
	meta.LastKnown = StatusComplete
	if err := m.Update(meta); err != nil {
		t.Fatal(err)
	}

	got, ok, err := m.Get(meta.ID)
	if err != nil || !ok {
		t.Fatalf("get after update: ok=%v err=%v", ok, err)
	}
	if got.Title != "fix the parser test" || got.LastKnown != StatusComplete {
		t.Fatalf("round trip = %+v", got)
	}
	if !got.UpdatedAt.After(meta.CreatedAt) && got.UpdatedAt.Equal(meta.CreatedAt) {
		t.Fatal("update did not refresh UpdatedAt")
	}
}

func TestTitleTruncatesInRunes(t *testing.T) {
	long := make([]rune, 200)
	for i := range long {
		long[i] = '世'
	}
	if got := Title(string(long)); len([]rune(got)) != TitleLimit {
		t.Fatalf("title runes = %d, want %d", len([]rune(got)), TitleLimit)
	}
	if got := Title(""); got != "" {
		t.Fatalf("empty title = %q", got)
	}
}

func TestGetRejectsTraversal(t *testing.T) {
	m, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if _, _, err := m.Get("../evil"); err == nil {
		t.Fatal("traversal id accepted")
	}
	if _, _, err := m.Get("a/b"); err == nil {
		t.Fatal("slash id accepted")
	}
}

func TestSecondManagerOnSameStoreDirFails(t *testing.T) {
	dir := t.TempDir()
	m, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if _, err := Open(dir); err == nil {
		t.Fatal("second manager on one store directory was allowed")
	}
}

func TestCacheRoundTripAtomicReplace(t *testing.T) {
	dir := t.TempDir()
	m, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	meta, err := m.Create("/w", "openai", "m")
	if err != nil {
		t.Fatal(err)
	}

	if _, ok, err := m.LoadCache(meta.ID); err != nil || ok {
		t.Fatalf("missing cache: ok=%v err=%v", ok, err)
	}
	cache := Cache{Head: 7, History: nil}
	if err := m.SaveCache(meta.ID, cache); err != nil {
		t.Fatal(err)
	}
	got, ok, err := m.LoadCache(meta.ID)
	if err != nil || !ok {
		t.Fatalf("load: ok=%v err=%v", ok, err)
	}
	if got.Head != 7 || got.Version != CacheVersion {
		t.Fatalf("round trip = %+v", got)
	}
	if got.SavedAt.IsZero() {
		t.Fatal("SavedAt not stamped")
	}

	// No temp leftovers after the atomic replace.
	entries, err := os.ReadDir(filepath.Join(dir, "sessions", meta.ID))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if len(e.Name()) > 0 && e.Name()[0] == '.' && e.Name() != "." && e.Name() != ".." {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

func TestCacheRejectsFutureVersion(t *testing.T) {
	dir := t.TempDir()
	m, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	meta, err := m.Create("/w", "openai", "m")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(dir, "sessions", meta.ID, "cache.json"),
		[]byte(`{"version":99,"head":3}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.LoadCache(meta.ID); err == nil {
		t.Fatal("future cache version accepted")
	}
}
