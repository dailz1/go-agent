// Package workspace confines file-tool access to one rooted directory.
package workspace

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"
)

const MaxFileBytes = 2 << 20

type Workspace struct {
	root    *os.Root
	path    string
	dataRel string
}

func Open(path, dataDir string) (*Workspace, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	r, err := os.OpenRoot(abs)
	if err != nil {
		return nil, err
	}
	w := &Workspace{root: r, path: abs}
	if dataDir != "" {
		data, err := canonicalDataPath(dataDir)
		if err != nil {
			r.Close()
			return nil, err
		}
		rel, err := filepath.Rel(abs, data)
		if err == nil && filepath.IsLocal(rel) {
			w.dataRel = rel
		}
	}
	return w, nil
}

func canonicalDataPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	suffix := ""
	for {
		resolved, err := filepath.EvalSymlinks(abs)
		if err == nil {
			return filepath.Join(resolved, suffix), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		suffix = filepath.Join(filepath.Base(abs), suffix)
		parent := filepath.Dir(abs)
		if parent == abs {
			return "", err
		}
		abs = parent
	}
}

func (w *Workspace) Close() error { return w.root.Close() }
func (w *Workspace) Path() string { return w.path }
func (w *Workspace) FS() fs.FS    { return w.root.FS() }

// Check rejects links in every existing component. os.Root also prevents escape
// if a component changes after this check; final opens use O_NOFOLLOW.
func (w *Workspace) Check(path string) error {
	if !filepath.IsLocal(path) || strings.ContainsRune(path, 0) {
		return errors.New("path must be workspace-relative")
	}
	parts := strings.Split(filepath.ToSlash(path), "/")
	current := ""
	for i, part := range parts {
		if part == ".." {
			return errors.New("parent traversal is not allowed")
		}
		current = filepath.Join(current, part)
		info, err := w.root.Lstat(current)
		if errors.Is(err, os.ErrNotExist) && i == len(parts)-1 {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("symbolic links are not allowed")
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return errors.New("special files are not allowed")
		}
	}
	return nil
}

func (w *Workspace) Stat(path string) (fs.FileInfo, error) {
	if err := w.Check(path); err != nil {
		return nil, err
	}
	return w.root.Lstat(path)
}

func (w *Workspace) Read(path string, limit int64) ([]byte, fs.FileInfo, error) {
	info, err := w.Stat(path)
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, nil, errors.New("not a regular file")
	}
	f, err := w.root.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, nil, errors.New("not a regular file or file exceeds byte limit")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, nil, err
	}
	if int64(len(data)) > limit {
		return nil, nil, errors.New("file exceeds byte limit")
	}
	if !Text(data) {
		return nil, nil, errors.New("file is not UTF-8 text")
	}
	return data, info, nil
}

func (w *Workspace) Writable(path string) error {
	if err := w.Check(path); err != nil {
		return err
	}
	clean := filepath.Clean(path)
	for _, part := range strings.Split(filepath.ToSlash(clean), "/") {
		if part == ".git" {
			return errors.New(".git is not writable")
		}
	}
	if w.dataRel != "" && (clean == w.dataRel || strings.HasPrefix(clean, w.dataRel+string(os.PathSeparator))) {
		return errors.New("harness data is not writable")
	}
	info, err := w.root.Lstat(clean)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("write target must be a regular file")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Nlink > 1 {
		return errors.New("multiple hard links are not writable")
	}
	return nil
}

func Hash(data []byte) string { return fmt.Sprintf("sha256:%x", sha256.Sum256(data)) }

func Text(data []byte) bool {
	return utf8.Valid(data) && !strings.ContainsRune(string(data), 0)
}

// Sensitive files are omitted by search and require explicit read approval.
func Sensitive(path string) bool {
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		lower := strings.ToLower(part)
		if lower == ".env" || strings.HasPrefix(lower, ".env.") || lower == ".ssh" ||
			lower == ".aws" || lower == ".npmrc" || lower == ".netrc" ||
			lower == "credentials" || strings.HasSuffix(lower, ".pem") || strings.HasSuffix(lower, ".key") {
			return true
		}
	}
	return false
}
