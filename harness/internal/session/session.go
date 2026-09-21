// Package session owns durable session identity, metadata and display
// caches under the harness data directory. It never interprets thread logs:
// kernel replay stays private, and the display cache is never model history.
package session

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dailz1/go-agent/store"
)

// Status is the last-known session outcome recorded in meta. It is a display
// hint derived from host observation, not a substitute for the thread log:
// after a crash it can be stale, and Open re-derives the truth read-only.
type Status string

const (
	StatusCreated   Status = "created"
	StatusRunning   Status = "running"
	StatusComplete  Status = "complete"
	StatusCancelled Status = "cancelled"
	StatusBroken    Status = "interrupted"
)

// Meta is the non-secret session record. The ID doubles as the kernel thread
// ID; it is persisted and fsynced before the first model call so a session
// that executed anything always has an identity on disk.
type Meta struct {
	ID        string    `json:"id"`
	Workspace string    `json:"workspace"`
	Provider  string    `json:"provider"`
	Model     string    `json:"model"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	LastKnown Status    `json:"last_known"`
}

// TitleLimit bounds the stored title in runes.
const TitleLimit = 80

// Manager owns the data-directory layout and the one JSONL store per
// process. Exactly one Manager may exist for a data directory because the
// kernel store enforces one live JSONL instance per directory.
type Manager struct {
	dataDir string
	st      *store.JSONLStore
	now     func() time.Time
}

// Layout under the data directory: sessions/<id>/meta.json and cache.json,
// shared store/, and per-session snapshots/<id>/ and outputs/<id>/.
func (m *Manager) SessionsDir() string { return filepath.Join(m.dataDir, "sessions") }
func (m *Manager) StoreDir() string    { return filepath.Join(m.dataDir, "store") }
func (m *Manager) SessionDir(id string) string {
	return filepath.Join(m.SessionsDir(), validID(id))
}
func (m *Manager) SnapshotDir(id string) string {
	return filepath.Join(m.dataDir, "snapshots", validID(id))
}
func (m *Manager) OutputDir(id string) string {
	return filepath.Join(m.dataDir, "outputs", validID(id))
}

func validID(id string) string {
	if id == "" || len(id) > 128 || strings.ContainsAny(id, "/\\") || filepath.Clean(id) != id {
		return ""
	}
	return id
}

// Open prepares the data directory and opens the shared JSONL store.
func Open(dataDir string) (*Manager, error) {
	if dataDir == "" {
		return nil, errors.New("session: data directory is required")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("session: create data directory: %w", err)
	}
	for _, dir := range []string{filepath.Join(dataDir, "sessions"), filepath.Join(dataDir, "store")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("session: create %s: %w", dir, err)
		}
	}
	st, err := store.NewJSONL(filepath.Join(dataDir, "store"))
	if err != nil {
		return nil, fmt.Errorf("session: open thread store: %w", err)
	}
	return &Manager{dataDir: dataDir, st: st, now: time.Now}, nil
}

// Store exposes the shared thread log. The Manager stays its only owner;
// closing happens through Close.
func (m *Manager) Store() store.Store { return m.st }

// Close releases the thread store. Session files stay on disk.
func (m *Manager) Close() error {
	if err := m.st.Close(); err != nil {
		return fmt.Errorf("session: close thread store: %w", err)
	}
	return nil
}

// NewID mints a session identity: crypto-random, kernel-thread-ID compatible.
func NewID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("session: generate id: %w", err)
	}
	return "s-" + hex.EncodeToString(b[:]), nil
}

// Create persists a new session identity before any execution can reference
// it. The caller fills workspace/provider/model; ID and timestamps are
// assigned here and the file is fsynced before Create returns.
func (m *Manager) Create(workspace, provider, model string) (Meta, error) {
	id, err := NewID()
	if err != nil {
		return Meta{}, err
	}
	now := m.now()
	meta := Meta{
		ID: id, Workspace: workspace, Provider: provider, Model: model,
		CreatedAt: now, UpdatedAt: now, LastKnown: StatusCreated,
	}
	if err := os.MkdirAll(m.SessionDir(id), 0o700); err != nil {
		return Meta{}, fmt.Errorf("session: create session directory: %w", err)
	}
	if err := m.writeMeta(meta); err != nil {
		return Meta{}, err
	}
	return meta, nil
}

// List scans session metadata. Threads are never enumerated from the store;
// a session exists exactly when its meta file does.
func (m *Manager) List() ([]Meta, error) {
	entries, err := os.ReadDir(m.SessionsDir())
	if err != nil {
		return nil, fmt.Errorf("session: scan sessions: %w", err)
	}
	metas := []Meta{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		meta, ok, err := m.Get(entry.Name())
		if err != nil {
			return nil, err
		}
		if ok {
			metas = append(metas, meta)
		}
	}
	sort.Slice(metas, func(i, j int) bool {
		if metas[i].UpdatedAt.Equal(metas[j].UpdatedAt) {
			return metas[i].ID < metas[j].ID
		}
		return metas[i].UpdatedAt.After(metas[j].UpdatedAt)
	})
	return metas, nil
}

// Get reads one session's metadata.
func (m *Manager) Get(id string) (Meta, bool, error) {
	if validID(id) == "" {
		return Meta{}, false, errors.New("session: invalid session id")
	}
	data, err := os.ReadFile(m.metaPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return Meta{}, false, nil
	}
	if err != nil {
		return Meta{}, false, fmt.Errorf("session: read meta: %w", err)
	}
	var meta Meta
	if err := json.Unmarshal(data, &meta); err != nil {
		return Meta{}, false, fmt.Errorf("session: decode meta %q: %w", id, err)
	}
	if meta.ID != id {
		return Meta{}, false, fmt.Errorf("session: meta id mismatch in %q", id)
	}
	return meta, true, nil
}

// Update persists metadata with a refreshed timestamp.
func (m *Manager) Update(meta Meta) error {
	if validID(meta.ID) == "" {
		return errors.New("session: invalid session id")
	}
	meta.UpdatedAt = m.now()
	return m.writeMeta(meta)
}

func (m *Manager) metaPath(id string) string {
	return filepath.Join(m.SessionDir(id), "meta.json")
}

// writeMeta durably replaces the metadata: 0600 temp file in the same
// directory, fsync, rename over the old file, then fsync the directory so
// the new name survives a crash.
func (m *Manager) writeMeta(meta Meta) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("session: encode meta: %w", err)
	}
	return atomicWrite(m.metaPath(meta.ID), data)
}

// Title derives the stored title from the session's first input: the
// first line only, truncated in runes.
func Title(input string) string {
	first := strings.TrimSpace(input)
	if i := strings.IndexRune(first, '\n'); i >= 0 {
		first = first[:i]
	}
	runes := []rune(strings.TrimSpace(first))
	if len(runes) > TitleLimit {
		runes = runes[:TitleLimit]
	}
	return strings.TrimSpace(string(runes))
}
