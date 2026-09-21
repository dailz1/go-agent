package snapshot

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

type binding struct {
	Version   int
	Workspace string
	ThreadID  string
}

func privateDirectory(path string) error {
	path, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		parent := filepath.Dir(path)
		if err := privateParent(parent); err != nil {
			return err
		}
		if err := os.Mkdir(path, 0700); err != nil {
			return err
		}
		return syncDirectory(parent)
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 {
		return errors.New("snapshot directory must be a private 0700 directory")
	}
	return privateParent(filepath.Dir(path))
}

func privateParent(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return privateDirectory(path)
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("snapshot directory parents must not be symbolic links")
	}
	if parent := filepath.Dir(path); parent != path {
		return privateParent(parent)
	}
	return nil
}

func syncDirectory(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (s *Store) load() error {
	want := binding{Version: 1, Workspace: s.w.Path(), ThreadID: s.thread}
	var got binding
	err := s.readJSON("binding.json", &got)
	if errors.Is(err, os.ErrNotExist) {
		if err := s.atomicJSON("binding.json", "binding", want); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if got != want {
		return ErrCredential
	}
	d, err := s.root.Open(".")
	if err != nil {
		return err
	}
	defer d.Close()
	entries, err := d.ReadDir(-1)
	if err != nil {
		return err
	}
	seen := map[Credential]bool{}
	for _, entry := range entries {
		name := entry.Name()
		if name == "binding.json" || strings.HasPrefix(name, ".tmp-") {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		if !validID(id) || name != id+".json" {
			return fmt.Errorf("invalid snapshot file %q", name)
		}
		var r Record
		if err := s.readJSON(name, &r); err != nil {
			return err
		}
		if r.Version != 1 || r.ID != id || r.Order == 0 || !s.validCredential(r.Credential) {
			return ErrCredential
		}
		if err := validChange(r.Change); err != nil {
			return err
		}
		if r.Change.Path != filepath.Clean(r.Change.Path) || seen[r.Credential] {
			return ErrCredential
		}
		switch r.State {
		case Prepared, Applied, RestorePrepared, Restored, Unapplied:
		default:
			return errors.New("unknown snapshot state")
		}
		seen[r.Credential] = true
		s.records = append(s.records, r)
		s.used += recordCost(r)
	}
	slices.SortFunc(s.records, func(a, b Record) int {
		if a.Order < b.Order {
			return -1
		}
		if a.Order > b.Order {
			return 1
		}
		return 0
	})
	for i := 1; i < len(s.records); i++ {
		if s.records[i-1].Order == s.records[i].Order {
			return ErrCredential
		}
	}
	return nil
}

func validID(id string) bool {
	if len(id) != 26 {
		return false
	}
	for _, r := range id {
		if !(r >= 'A' && r <= 'Z') && !(r >= '2' && r <= '7') {
			return false
		}
	}
	return true
}

func (s *Store) readJSON(name string, dest any) error {
	f, err := s.root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	const maxRecord = 25 << 20
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > maxRecord {
		return errors.New("snapshot must be a private bounded regular file")
	}
	dec := json.NewDecoder(io.LimitReader(f, maxRecord+1))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dest); err != nil {
		return err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return errors.New("trailing snapshot data")
	}
	return nil
}

// Quota includes encoded images and manifest metadata, reserving enough space
// for every state name so restoring never needs additional quota.
func recordCost(r Record) int64 {
	r.State = RestorePrepared
	b, _ := json.Marshal(r)
	return int64(len(b))
}

func (s *Store) save(r Record) error {
	err := s.atomicJSON(r.ID+".json", string(r.State), r)
	if err != nil {
		s.failed = err
		return fmt.Errorf("persist snapshot %s: %w", r.State, err)
	}
	return nil
}

func (s *Store) boundary(point string) error {
	if s.fault != nil {
		return s.fault(point)
	}
	return nil
}

func (s *Store) atomicJSON(name, phase string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	tmp := ".tmp-" + rand.Text()
	f, err := s.root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer s.root.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	for _, step := range []struct {
		name string
		fn   func() error
	}{
		{name: "file-sync", fn: f.Sync},
		{name: "rename", fn: func() error {
			if err := f.Close(); err != nil {
				return err
			}
			return s.root.Rename(tmp, name)
		}},
		{name: "dir-sync", fn: func() error {
			d, err := s.root.Open(".")
			if err != nil {
				return err
			}
			defer d.Close()
			return d.Sync()
		}},
	} {
		if err := s.boundary(phase + "." + step.name + ".before"); err != nil {
			f.Close()
			return err
		}
		if err := step.fn(); err != nil {
			f.Close()
			return err
		}
		if err := s.boundary(phase + "." + step.name + ".after"); err != nil {
			f.Close()
			return err
		}
	}
	return nil
}
