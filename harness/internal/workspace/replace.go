package workspace

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Change is the immutable before/after image passed to the Stage C snapshot gate.
type Change struct {
	Path   string
	Before string
	After  string
	Exists bool
	Mode   fs.FileMode
}

// Verify checks existence, contents, mode and write policy without side effects.
func (w *Workspace) Verify(change Change) error {
	if err := w.Writable(change.Path); err != nil {
		return err
	}
	data, info, err := w.Read(change.Path, MaxFileBytes)
	if !change.Exists && errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !change.Exists || string(data) != change.Before || info.Mode().Perm() != change.Mode.Perm() {
		return errors.New("file changed since read; read again")
	}
	return nil
}

// Replace is called only by the file tool's approval-and-snapshot callback.
// The caller owns snapshot durability. No external concurrent writer is allowed
// between the final check and rename; this is not a filesystem transaction.
func (w *Workspace) Replace(ctx context.Context, change Change) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(change.After) > MaxFileBytes || !Text([]byte(change.After)) {
		return errors.New("replacement must be UTF-8 text at most 2 MiB")
	}
	if err := w.Verify(change); err != nil {
		return err
	}
	parent, err := w.root.OpenRoot(filepath.Dir(change.Path))
	if err != nil {
		return err
	}
	defer parent.Close()
	name := ".go-agent-" + rand.Text()
	f, err := parent.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer parent.Remove(name)
	mode := change.Mode.Perm()
	if !change.Exists {
		mode = 0644
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if _, err := f.WriteString(change.After); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := w.Verify(change); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := parent.Rename(name, filepath.Base(change.Path)); err != nil {
		return err
	}
	dir, err := parent.Open(".")
	if err != nil {
		return fmt.Errorf("open changed directory for sync: %w", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync changed directory: %w", err)
	}
	return nil
}
