package store

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

func TestJSONLInteriorCorruptionFailsLoudly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dir := newTestJSONL(t)
	if _, err := s.Append(ctx, "t", 0, rec("a", KindRunStarted), rec("b", KindAgentEvent)); err != nil {
		t.Fatalf("append: %v", err)
	}
	path := filepath.Join(dir, "t.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// Corrupt the FIRST (interior) line, keeping the file well-formed line-wise.
	lines := splitLines(data)
	lines[0] = []byte("not-json")
	if err := os.WriteFile(path, joinLines(lines), 0o644); err != nil {
		t.Fatalf("write corrupted: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := NewJSONL(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if _, err := reopened.Latest(ctx, "t"); !errors.Is(err, ErrCorruptLog) {
		t.Errorf("interior corruption error = %v, want ErrCorruptLog", err)
	}
}

func TestJSONLBlankLineIsCorruption(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, err := NewJSONL(dir)
	if err != nil {
		t.Fatalf("NewJSONL: %v", err)
	}
	if _, err := s.Append(context.Background(), "t", 0, rec("a", KindRunStarted)); err != nil {
		t.Fatalf("append: %v", err)
	}
	path := filepath.Join(dir, "t.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// A silently dropped record would shorten the log; a blank line must
	// surface as corruption instead.
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := NewJSONL(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if _, err := reopened.Latest(context.Background(), "t"); !errors.Is(err, ErrCorruptLog) {
		t.Errorf("blank line error = %v, want ErrCorruptLog", err)
	}
}

func TestJSONLSequenceGapFailsLoudly(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, err := NewJSONL(dir)
	if err != nil {
		t.Fatalf("NewJSONL: %v", err)
	}
	defer s.Close()
	r := rec("a", KindAgentEvent)
	r.Seq = 5 // a hand-forged log with a gap
	if err := writeRawRecord(dir, "t", r); err != nil {
		t.Fatalf("write raw: %v", err)
	}
	if _, err := s.Latest(context.Background(), "t"); !errors.Is(err, ErrCorruptLog) {
		t.Errorf("sequence gap error = %v, want ErrCorruptLog", err)
	}
}

func TestJSONLUnsupportedSchemaFailsLoudly(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, err := NewJSONL(dir)
	if err != nil {
		t.Fatalf("NewJSONL: %v", err)
	}
	defer s.Close()
	r := rec("a", KindAgentEvent)
	r.Schema = SchemaV1 + 99
	if err := writeRawRecord(dir, "t", r); err != nil {
		t.Fatalf("write raw: %v", err)
	}
	if _, err := s.Latest(context.Background(), "t"); !errors.Is(err, ErrUnsupportedSchema) {
		t.Errorf("unsupported schema error = %v, want ErrUnsupportedSchema", err)
	}
}

func TestJSONLDuplicateRecordIDIsCorruption(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, err := NewJSONL(dir)
	if err != nil {
		t.Fatalf("NewJSONL: %v", err)
	}
	defer s.Close()
	r1 := rec("dup", KindAgentEvent)
	r1.Seq = 0
	r2 := rec("dup", KindAgentEvent)
	r2.Seq = 1
	if err := writeRawRecord(dir, "t", r1); err != nil {
		t.Fatalf("write raw: %v", err)
	}
	if err := appendRawRecord(dir, "t", r2); err != nil {
		t.Fatalf("write raw: %v", err)
	}
	if _, err := s.Latest(context.Background(), "t"); !errors.Is(err, ErrCorruptLog) {
		t.Errorf("duplicate id error = %v, want ErrCorruptLog", err)
	}
}

func writeRawRecord(dir, thread string, r Record) error {
	return appendRawRecord(dir, thread, r)
}

func appendRawRecord(dir, thread string, r Record) error {
	line, err := json.Marshal(r)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, url.PathEscape(thread)+".jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

func splitLines(data []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i, b := range data {
		if b == '\n' {
			lines = append(lines, data[start:i])
			start = i + 1
		}
	}
	if start < len(data) {
		lines = append(lines, data[start:])
	}
	return lines
}

func joinLines(lines [][]byte) []byte {
	var out []byte
	for _, l := range lines {
		out = append(out, l...)
		out = append(out, '\n')
	}
	return out
}
