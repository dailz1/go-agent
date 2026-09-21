package snapshot

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/dailz1/go-agent/harness/internal/workspace"
)

const DefaultQuota int64 = 256 << 20

var (
	ErrCredential = errors.New("snapshot credential does not match")
	ErrQuota      = errors.New("snapshot session quota exceeded")
	ErrConflict   = errors.New("snapshot file conflicts with recorded image")
	ErrNotLatest  = errors.New("restore the latest change for this path first")
	ErrUncertain  = errors.New("snapshot persistence uncertain; close and reopen before modifying")
	ErrNotFound   = errors.New("snapshot change not found")
)

type State string

const (
	Prepared        State = "prepared"
	Applied         State = "applied"
	RestorePrepared State = "restore-prepared"
	Restored        State = "restored"
	Unapplied       State = "unapplied"
)

// Credential is supplied by the host, not tool arguments. Sequence is unique
// within a generation; the host must not reuse generations after restart.
type Credential struct {
	Workspace    string
	ThreadID     string
	Generation   uint64
	ToolSequence uint64
}

type Record struct {
	Version    int
	ID         string
	Order      uint64
	Credential Credential
	Change     workspace.Change
	State      State
}

// Store belongs to one session. The host owns the workspace process lock and
// serializes restore with runs; this mutex only serializes this Store's methods.
type Store struct {
	mu      sync.Mutex
	dir     string
	root    *os.Root
	w       *workspace.Workspace
	thread  string
	quota   int64
	used    int64
	records []Record
	failed  error
	closed  bool
	fault   func(string) error
}

func Open(dir string, w *workspace.Workspace, threadID string, quotaBytes int64) (*Store, error) {
	if w == nil || threadID == "" {
		return nil, ErrCredential
	}
	if quotaBytes <= 0 {
		quotaBytes = DefaultQuota
	}
	if err := privateDirectory(dir); err != nil {
		return nil, fmt.Errorf("open snapshot directory: %w", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	s := &Store{dir: dir, root: root, w: w, thread: threadID, quota: quotaBytes, records: []Record{}}
	if err := s.load(); err != nil {
		root.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.root.Close()
}

// List returns immutable images and persisted states, not a claim about current
// disk contents. Prepared records remain uncertain until an explicit Restore.
func (s *Store) List(ctx context.Context) ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.closed {
		return nil, os.ErrClosed
	}
	return slices.Clone(s.records), nil
}

// Commit durably prepares a snapshot before calling apply once. Apply must make
// exactly change via Workspace.Replace, and must not reenter this Store.
// Any error after preparation retains evidence; Commit never retries apply.
func (s *Store) Commit(
	ctx context.Context, cred Credential, change workspace.Change, apply func() error,
) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(ctx); err != nil {
		return Record{}, err
	}
	if !s.validCredential(cred) || apply == nil {
		return Record{}, ErrCredential
	}
	for _, r := range s.records {
		if r.Credential == cred {
			return Record{}, ErrCredential
		}
	}
	if err := validChange(change); err != nil {
		return Record{}, err
	}
	if err := s.w.Verify(change); err != nil {
		return Record{}, fmt.Errorf("%w: %w", ErrConflict, err)
	}
	change.Path = filepath.Clean(change.Path)
	if change.Exists && change.Before == change.After {
		return Record{}, nil
	}
	// An unresolved write/restore must be reconciled before another edit can
	// hide its disk evidence. Different paths remain independently usable.
	for _, r := range s.records {
		if r.Change.Path == change.Path && (r.State == Prepared || r.State == RestorePrepared) {
			return Record{}, ErrUncertain
		}
	}
	order := uint64(1)
	if len(s.records) > 0 {
		order = s.records[len(s.records)-1].Order + 1
	}
	r := Record{Version: 1, ID: rand.Text(), Order: order, Credential: cred, Change: change, State: Prepared}
	cost := recordCost(r)
	if cost > s.quota-s.used {
		return Record{}, ErrQuota
	}
	if err := s.save(r); err != nil {
		return r, err
	}
	s.used += cost
	s.records = append(s.records, r)
	if err := s.ready(ctx); err != nil {
		return r, err
	}
	if err := s.w.Verify(change); err != nil {
		return r, fmt.Errorf("%w: %w", ErrConflict, err)
	}
	if err := apply(); err != nil {
		return r, err
	}
	if err := s.w.Verify(afterImage(change)); err != nil {
		return r, fmt.Errorf("%w: %w", ErrConflict, err)
	}
	// Once apply returns, persist its outcome even if cancellation just won.
	r.State = Applied
	if err := s.save(r); err != nil {
		r.State = Prepared
		return r, err
	}
	s.records[len(s.records)-1] = r
	return r, nil
}

func (s *Store) ready(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.closed {
		return os.ErrClosed
	}
	if s.failed != nil {
		return fmt.Errorf("%w: %w", ErrUncertain, s.failed)
	}
	return nil
}

func (s *Store) validCredential(c Credential) bool {
	return c.Workspace == s.w.Path() && c.ThreadID == s.thread && c.Generation > 0 && c.ToolSequence > 0
}

func validChange(c workspace.Change) error {
	if c.Path == "" || !filepath.IsLocal(c.Path) {
		return ErrCredential
	}
	if c.Mode != c.Mode.Perm() || (!c.Exists && c.Before != "") {
		return ErrCredential
	}
	if len(c.Before) > workspace.MaxFileBytes || len(c.After) > workspace.MaxFileBytes {
		return errors.New("snapshot image exceeds file size limit")
	}
	if !workspace.Text([]byte(c.Before)) || !workspace.Text([]byte(c.After)) {
		return errors.New("snapshot images must be UTF-8 text")
	}
	return nil
}

func afterImage(c workspace.Change) workspace.Change {
	mode := c.Mode
	if !c.Exists {
		mode = 0644
	}
	return workspace.Change{Path: c.Path, Before: c.After, Exists: true, Mode: mode}
}
