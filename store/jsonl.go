package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
)

// ErrClosed is returned by JSONLStore methods after Close.
var ErrClosed = errors.New("store: jsonl store closed")

// jsonlRegistry enforces one live JSONLStore per physical directory per
// process. Locks are instance-local, so two instances on one directory could
// append duplicate sequences; the registry turns that documented
// single-process restriction into an enforced error instead of silent
// corruption.
var jsonlRegistry = struct {
	mu   sync.Mutex
	dirs map[string]*JSONLStore
}{dirs: make(map[string]*JSONLStore)}

// JSONLStore is a file-backed Store: one newline-delimited JSON file per
// thread inside a directory.
//
// Durability contract: every acknowledged append is fsynced to the file
// before Append returns (file creation also syncs the directory once). On
// first access after a crash, a torn final line (no trailing newline, or
// trailing garbage) is repaired by truncation and the truncation is synced;
// interior corruption and sequence gaps are NOT repaired — they surface as
// ErrCorruptLog. Records with a schema newer than SchemaV1 surface as
// ErrUnsupportedSchema. If a write or sync fails, the thread is reloaded
// from disk before the error returns, so the in-memory view always matches
// durable bytes and retries classify correctly.
//
// Lifecycle: exactly one live JSONLStore may exist per physical directory
// (symlink aliases resolve to the same registration; enforced). Close marks
// the store closed first — new operations fail with ErrClosed — then waits
// for every admitted operation to finish before closing file handles, and
// only then unregisters the directory. Concurrent and repeated Close calls
// join the same drain: none of them returns until the drain and unregister
// have completed. Cross-process access is unsupported (v1); within one
// process, per-thread writes are serialized.
type JSONLStore struct {
	dir string
	mu  sync.Mutex
	// closed is an atomic so thread admission can recheck it while holding
	// only the thread lock (avoiding th.mu -> s.mu lock inversion).
	closed    atomic.Bool
	threads   map[string]*jsonlThread
	drainDone chan struct{} // closed after the drain and unregister complete
	drainErr  error         // the first closer's drain result
}

type jsonlThread struct {
	mu      sync.Mutex
	path    string
	loaded  bool
	records []Record
	file    *os.File
}

// NewJSONL returns a JSONL-backed Store rooted at dir. The directory is
// created if missing. Only one live instance may exist per physical
// directory; creating a second — including through a symlink alias —
// returns an error until the first is closed.
func NewJSONL(dir string) (*JSONLStore, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("store: resolve directory: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("store: create directory: %w", err)
	}
	// Canonicalize to the physical directory so symlink aliases cannot
	// bypass the one-instance rule.
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		// Failing open would register a lexical alias under a different key
		// and defeat the one-instance rule.
		return nil, fmt.Errorf("store: canonicalize directory: %w", err)
	}
	abs = resolved

	jsonlRegistry.mu.Lock()
	defer jsonlRegistry.mu.Unlock()
	if existing := jsonlRegistry.dirs[abs]; existing != nil {
		return nil, fmt.Errorf("store: jsonl store already open for %s", abs)
	}
	s := &JSONLStore{dir: abs, threads: make(map[string]*jsonlThread), drainDone: make(chan struct{})}
	jsonlRegistry.dirs[abs] = s
	return s, nil
}

// Close releases all open file handles and unregisters the store, allowing
// a new instance on the same directory. Using the store afterwards returns
// ErrClosed.
//
// The lifecycle is linearized: closed is set under s.mu (new operations are
// rejected from that point), then every admitted operation is drained by
// waiting on its thread lock, and the directory is unregistered only after
// the drain. A repeated Close never unregisters a newer store: the registry
// entry is removed only while it still points at this instance.
func (s *JSONLStore) Close() error {
	if s.closed.Swap(true) {
		// Another caller owns the drain: join it. No Close — first or
		// later — returns before the drain and unregister have completed.
		<-s.drainDone
		return s.drainErr
	}
	s.mu.Lock()
	draining := make([]*jsonlThread, 0, len(s.threads))
	for _, th := range s.threads {
		draining = append(draining, th)
	}
	s.mu.Unlock()

	var firstErr error
	for _, th := range draining {
		th.mu.Lock()
		if th.file != nil {
			if err := th.file.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
			th.file = nil
		}
		th.mu.Unlock()
	}

	jsonlRegistry.mu.Lock()
	if jsonlRegistry.dirs[s.dir] == s {
		delete(jsonlRegistry.dirs, s.dir)
	}
	jsonlRegistry.mu.Unlock()

	s.drainErr = firstErr
	close(s.drainDone)
	return firstErr
}

func (s *JSONLStore) checkOpen() error {
	if s.closed.Load() {
		return ErrClosed
	}
	return nil
}

var _ Store = (*JSONLStore)(nil)
