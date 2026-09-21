package workspace

import (
	"context"
	"path/filepath"
)

// Restore reverses one recorded Replace, only while disk still matches its
// after-image. The caller must durably prepare restoration first and exclude
// other harness operations; arbitrary concurrent external writers are unsupported.
func (w *Workspace) Restore(ctx context.Context, original Change) error {
	mode := original.Mode.Perm()
	if !original.Exists {
		mode = 0644
	}
	reverse := Change{
		Path: original.Path, Before: original.After, After: original.Before,
		Exists: true, Mode: mode,
	}
	if original.Exists {
		return w.Replace(ctx, reverse)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := w.Verify(reverse); err != nil {
		return err
	}
	parent, err := w.root.OpenRoot(filepath.Dir(original.Path))
	if err != nil {
		return err
	}
	defer parent.Close()
	if err := w.Verify(reverse); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := parent.Remove(filepath.Base(original.Path)); err != nil {
		return err
	}
	d, err := parent.Open(".")
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// SyncParent settles a previously attempted restore whose rename or removal
// landed but whose directory sync may have failed.
func (w *Workspace) SyncParent(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := w.Writable(path); err != nil {
		return err
	}
	d, err := w.root.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
