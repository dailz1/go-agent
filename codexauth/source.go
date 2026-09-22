package codexauth

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/dailz1/go-agent/llm/codex/auth"
)

var (
	ErrBusy   = errors.New("codex credentials are already in use")
	ErrClosed = errors.New("codex credential source is closed")
)

// FileSource holds exclusive ownership of an independent credential store.
// Share it within a process and Close it after all providers have stopped.
type FileSource struct {
	mu     sync.RWMutex
	source *auth.RefreshingSource
	lock   *fileLock
	closed bool
}

// OpenFileSource locks and loads the store. OAuth endpoints default to the fixed
// login profile. The supplied Persist callback is replaced by the file store.
func OpenFileSource(path string, cfg auth.RefreshConfig) (*FileSource, error) {
	lock, err := acquireLock(path)
	if err != nil {
		return nil, err
	}
	state, err := Load(path)
	if err != nil {
		return nil, errors.Join(err, lock.close())
	}
	cfg = refreshConfig(cfg)
	cfg.Persist = func(ctx context.Context, next auth.State) error {
		return save(ctx, path, next)
	}
	source, err := auth.NewRefreshingSource(state, cfg)
	if err != nil {
		return nil, errors.Join(err, lock.close())
	}
	return &FileSource{source: source, lock: lock}, nil
}

func (s *FileSource) Token(ctx context.Context) (auth.Token, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return auth.Token{}, ErrClosed
	}
	return s.source.Token(ctx)
}

func (s *FileSource) Refresh(ctx context.Context, rejected *auth.Token) (auth.Token, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return auth.Token{}, ErrClosed
	}
	return s.source.Refresh(ctx, rejected)
}

// Close waits for active calls and releases only this instance's lock.
func (s *FileSource) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.lock.close()
}

type lockOwner struct {
	Owner string `json:"owner"`
	PID   int    `json:"pid"`
}

type fileLock struct {
	path  string
	owner lockOwner
}

func acquireLock(path string) (*fileLock, error) {
	if err := ownStorePath(path); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create credential directory: %w", err)
	}
	lock := &fileLock{path: path + ".lock", owner: lockOwner{Owner: rand.Text(), PID: os.Getpid()}}
	file, err := os.OpenFile(lock.path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("%w: %s; remove a stale lock only after confirming its owner has stopped", ErrBusy, lock.path)
	}
	if err != nil {
		return nil, fmt.Errorf("lock codex credentials: %w", err)
	}
	data, err := json.Marshal(lock.owner)
	if err == nil {
		_, err = file.Write(data)
	}
	closeErr := file.Close()
	if err = errors.Join(err, closeErr); err != nil {
		return nil, errors.Join(err, os.Remove(lock.path))
	}
	return lock, nil
}

func (l *fileLock) close() error {
	data, err := os.ReadFile(l.path)
	if err != nil {
		return fmt.Errorf("read credential lock: %w", err)
	}
	var current lockOwner
	if err := json.Unmarshal(data, &current); err != nil || current != l.owner {
		return errors.New("credential lock ownership changed; refusing to remove it")
	}
	if err := os.Remove(l.path); err != nil {
		return fmt.Errorf("release credential lock: %w", err)
	}
	return nil
}

func refreshConfig(cfg auth.RefreshConfig) auth.RefreshConfig {
	if cfg.ClientID == "" {
		cfg.ClientID = auth.ClientID
	}
	if cfg.TokenURL == "" {
		cfg.TokenURL = auth.Issuer + "/oauth/token"
	}
	return cfg
}
