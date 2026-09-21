package tools

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"syscall"
)

const maxOutputBytes int64 = 64 << 20
const maxInlineBytes = 8 << 10

var errOutputLimit = errors.New("shell output limit reached")
var outputIDPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

// OutputStore retains raw command output for the owning session's lifetime.
// Close releases the directory handle; it never removes saved output.
type OutputStore struct {
	dir      string
	root     *os.Root
	maxBytes int64
	newFile  func(string) (*os.File, error)
}

func OpenOutputStore(dir string) (*OutputStore, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create outputs: %w", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open outputs: %w", err)
	}
	s := &OutputStore{dir: dir, root: root, maxBytes: maxOutputBytes}
	s.newFile = func(id string) (*os.File, error) {
		return root.OpenFile(id, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	}
	return s, nil
}

func (s *OutputStore) Close() error { return s.root.Close() }

func (s *OutputStore) create(inline int) (*outputCapture, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, err
	}
	id := hex.EncodeToString(random[:])
	f, err := s.newFile(id)
	if err != nil {
		return nil, fmt.Errorf("create output: %w", err)
	}
	dir, err := s.root.Open(".")
	if err == nil {
		err = errors.Join(dir.Sync(), dir.Close())
	}
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return &outputCapture{file: f, id: id, limit: s.maxBytes, inline: inline}, nil
}

// ReadOutput returns numbered lines without loading the full output. Lines over
// 8 KiB are split into numbered 8 KiB fragments, so even a newline-free log can
// be paginated in full. Raw bytes, including invalid UTF-8, stay in the file.
func (s *OutputStore) ReadOutput(ctx context.Context, id string, offset, limit int) (ReadObservation, error) {
	result := ReadObservation{OutputID: id, Lines: []Line{}}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if !outputIDPattern.MatchString(id) {
		return result, errors.New("invalid output ID")
	}
	if err := page(offset, limit); err != nil {
		return result, err
	}
	f, err := s.root.OpenFile(id, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return result, err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return result, err
	}
	if !stat.Mode().IsRegular() {
		return result, errors.New("output is not a regular file")
	}
	result.Exists = true
	reader := bufio.NewReaderSize(io.LimitReader(f, maxOutputBytes), maxInlineBytes)
	size := 0
	for number := 1; ; number++ {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		part, err := reader.ReadSlice('\n')
		if err != nil && err != io.EOF && err != bufio.ErrBufferFull {
			return result, err
		}
		if len(part) == 0 && err == io.EOF {
			break
		}
		if number >= offset {
			if len(result.Lines) == limit || size+len(part)+32 > observationBytes {
				result.Truncated, result.NextOffset = true, number
				break
			}
			text := strings.TrimSuffix(strings.TrimSuffix(string(part), "\n"), "\r")
			result.Lines = append(result.Lines, Line{Number: number, Text: text})
			size += len(part) + 32
		}
		if err == io.EOF {
			break
		}
	}
	return result, nil
}

// One collector goroutine owns a capture until its completion signal is joined.
type outputCapture struct {
	file   *os.File
	id     string
	limit  int64
	inline int
	bytes  int64
	head   []byte
	tail   []byte
}

func (c *outputCapture) Write(data []byte) (int, error) {
	remaining := c.limit - c.bytes
	if int64(len(data)) > remaining {
		data = data[:remaining]
	}
	n, err := c.file.Write(data)
	c.bytes += int64(n)
	data = data[:n]
	headSize := (c.inline + 1) / 2
	tailSize := c.inline / 2
	if len(c.head) < headSize {
		take := min(headSize-len(c.head), len(data))
		c.head = append(c.head, data[:take]...)
		data = data[take:]
	}
	if tailSize > 0 {
		if len(data) >= tailSize {
			c.tail = append(c.tail[:0], data[len(data)-tailSize:]...)
		} else {
			excess := max(0, len(c.tail)+len(data)-tailSize)
			c.tail = append(c.tail[excess:], data...)
		}
	}
	if err != nil {
		return n, fmt.Errorf("write output: %w", err)
	}
	if c.bytes >= c.limit {
		return n, errOutputLimit
	}
	return n, nil
}

func (c *outputCapture) close() error {
	return errors.Join(c.file.Sync(), c.file.Close())
}
