package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/dailz1/go-agent/llm"
)

// CacheVersion is the display-cache schema. An unknown future version is
// rejected rather than best-effort interpreted.
const CacheVersion = 1

// Cache is the display snapshot of one session view: the authoritative
// History captured with the store head it was consistent with, saved with
// the public llm JSON codec. It exists only for rendering; it is never fed
// back into model execution and never replaces kernel replay.
type Cache struct {
	Version int           `json:"version"`
	Head    int64         `json:"head"`
	History []llm.Message `json:"history"`
	SavedAt time.Time     `json:"saved_at"`
}

// LoadCache reads the display cache. A missing cache is not an error; the
// caller renders an honest gap instead.
func (m *Manager) LoadCache(id string) (Cache, bool, error) {
	if validID(id) == "" {
		return Cache{}, false, errors.New("session: invalid session id")
	}
	data, err := os.ReadFile(filepath.Join(m.SessionDir(id), "cache.json"))
	if errors.Is(err, os.ErrNotExist) {
		return Cache{}, false, nil
	}
	if err != nil {
		return Cache{}, false, fmt.Errorf("session: read cache: %w", err)
	}
	var cache Cache
	if err := json.Unmarshal(data, &cache); err != nil {
		return Cache{}, false, fmt.Errorf("session: decode cache %q: %w", id, err)
	}
	if cache.Version != CacheVersion {
		return Cache{}, false, fmt.Errorf("session: cache %q has unsupported version %d", id, cache.Version)
	}
	return cache, true, nil
}

// SaveCache durably replaces the display cache. A failed save leaves any
// previous cache intact; the caller must not report the view as saved.
func (m *Manager) SaveCache(id string, cache Cache) error {
	if validID(id) == "" {
		return errors.New("session: invalid session id")
	}
	cache.Version = CacheVersion
	cache.SavedAt = m.now()
	data, err := json.Marshal(cache)
	if err != nil {
		return fmt.Errorf("session: encode cache: %w", err)
	}
	return atomicWrite(filepath.Join(m.SessionDir(id), "cache.json"), data)
}

// atomicWrite installs data at path with 0600 permissions: a temp file in
// the same directory, fsync, rename, then a directory fsync so the rename
// itself is durable.
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("session: create temp file: %w", err)
	}
	name := tmp.Name()
	defer func() {
		if name != "" {
			tmp.Close()
			os.Remove(name)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("session: set temp permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("session: write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("session: sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("session: close temp file: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("session: rename into place: %w", err)
	}
	name = "" // renamed; the deferred cleanup must not remove the target
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("session: open directory for sync: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("session: sync directory: %w", err)
	}
	return nil
}
