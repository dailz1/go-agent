package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
)

// load reads the thread file, repairing a torn final line (durably) and
// failing loudly on structural corruption, duplicate record IDs, sequence
// gaps, or unsupported schema versions.
func (th *jsonlThread) load() error {
	data, err := os.ReadFile(th.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // fresh thread; file is created on first append
		}
		return fmt.Errorf("store: read log: %w", err)
	}

	var records []Record
	seen := make(map[string]struct{})
	var complete int // bytes of the valid prefix, including trailing newlines
	for len(data) > complete {
		end := bytes.IndexByte(data[complete:], '\n')
		if end < 0 {
			// Torn final line: no trailing newline. Truncate the file to the
			// valid prefix and sync, so the repair itself survives a crash.
			if err := truncateAndSync(th.path, int64(complete)); err != nil {
				return err
			}
			break
		}
		line := data[complete : complete+end]
		complete += end + 1
		if len(bytes.TrimSpace(line)) == 0 {
			return fmt.Errorf("%w: blank line at byte %d", ErrCorruptLog, complete)
		}
		var r Record
		if err := json.Unmarshal(line, &r); err != nil {
			return fmt.Errorf("%w: invalid record at byte %d: %v", ErrCorruptLog, complete, err)
		}
		if r.Schema > SchemaV2 || (r.Kind == KindRunCancelled && r.Schema != SchemaV2) {
			return ErrUnsupportedSchema
		}
		if r.Schema < 1 || r.ID == "" {
			return fmt.Errorf("%w: structurally invalid record at byte %d", ErrCorruptLog, complete)
		}
		if _, dup := seen[r.ID]; dup {
			return fmt.Errorf("%w: duplicate record id %q at byte %d", ErrCorruptLog, r.ID, complete)
		}
		seen[r.ID] = struct{}{}
		if r.Kind == KindCheckpoint && len(r.Payload) == 0 {
			return fmt.Errorf("%w: checkpoint without payload at byte %d", ErrCorruptLog, complete)
		}
		records = append(records, r)
	}
	for i, r := range records {
		if r.Seq != int64(i) {
			return fmt.Errorf("%w: sequence %d at position %d", ErrCorruptLog, r.Seq, i)
		}
	}
	th.records = records
	return nil
}

// truncateAndSync durably repairs a torn tail: the size change is synced
// before the repair is considered done.
func truncateAndSync(path string, size int64) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("store: repair torn tail: %w", err)
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		return fmt.Errorf("store: repair torn tail: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("store: sync repaired tail: %w", err)
	}
	return nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("store: sync directory: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("store: sync directory: %w", err)
	}
	return nil
}
