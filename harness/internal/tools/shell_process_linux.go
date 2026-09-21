package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func (s *Shell) run(ctx context.Context, command string, dir *os.File, timeout time.Duration) (*ShellObservation, error) {
	// cmd.Dir needs a path: the child chdirs before exec wires ExtraFiles, so
	// /proc/self/fd/N is not valid there. Resolve the pinned descriptor instead;
	// the descriptor keeps the inode the rooted open verified, and readlink
	// reports its live name.
	dirPath, err := resolveDirPath(dir)
	if err != nil {
		return nil, err
	}
	capture, err := s.outputs.create(s.inline)
	if err != nil {
		return nil, err
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, errors.Join(err, capture.close())
	}
	defer reader.Close()
	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.Dir = dirPath
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout, cmd.Stderr = writer, writer
	// A nil Stdin is /dev/null, not the harness terminal.
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if !s.credentialEnv[name] {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	if cmd.Env == nil {
		cmd.Env = []string{}
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(err, writer.Close(), capture.close())
	}
	if err := cmd.Start(); err != nil {
		return nil, errors.Join(fmt.Errorf("start shell: %w", err), writer.Close(), capture.close())
	}
	closeErr := writer.Close()
	waited := make(chan error, 1)
	copied := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	go func() {
		_, err := io.Copy(capture, reader)
		copied <- err
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	outcome := "exit"
	var waitErr, copyErr error
	waitDone, copyDone := false, false
	select {
	case waitErr = <-waited:
		waitDone = true
	case copyErr = <-copied:
		copyDone = true
		if copyErr == nil {
			select {
			case waitErr = <-waited:
				waitDone = true
			case <-ctx.Done():
				outcome = "cancelled"
			case <-timer.C:
				outcome = "timeout"
			}
		}
	case <-ctx.Done():
		outcome = "cancelled"
	case <-timer.C:
		outcome = "timeout"
	}
	cleanupErr := s.cleanGroup(cmd.Process.Pid)
	if !waitDone {
		select {
		case waitErr = <-waited:
		case <-time.After(s.killGrace):
			cleanupErr = errors.Join(cleanupErr, errors.New("shell wait did not finish"))
		}
	}
	if !copyDone {
		select {
		case copyErr = <-copied:
		case <-time.After(s.killGrace):
			cleanupErr = errors.Join(cleanupErr, errors.New("output collector did not finish"))
			reader.Close()
			copyErr = <-copied
		}
	}
	saveErr := capture.close()
	if cleanupErr != nil {
		s.cleanupErr = &CleanupError{PGID: cmd.Process.Pid, Cause: cleanupErr}
		return nil, errors.Join(s.cleanupErr, ctx.Err(), saveErr, closeErr)
	}
	if errors.Is(copyErr, errOutputLimit) {
		outcome = "output_limit"
		copyErr = nil
	}
	if err := errors.Join(copyErr, saveErr, closeErr); err != nil {
		return nil, fmt.Errorf("collect shell output: %w", err)
	}
	exitCode := -1
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	var exitErr *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exitErr) {
		return nil, fmt.Errorf("wait shell: %w", waitErr)
	}
	if ctx.Err() != nil {
		outcome = "cancelled"
	}
	got := &ShellObservation{
		OutputID: capture.id, Outcome: outcome, ExitCode: exitCode, Bytes: capture.bytes,
		Head: string(capture.head), Tail: string(capture.tail),
		Truncated: capture.bytes > int64(len(capture.head)+len(capture.tail)),
	}
	return got, ctx.Err()
}

func signalGroup(pgid int, signal syscall.Signal) error {
	err := syscall.Kill(-pgid, signal)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func resolveDirPath(f *os.File) (string, error) {
	target, err := os.Readlink("/proc/self/fd/" + strconv.Itoa(int(f.Fd())))
	if err != nil {
		return "", fmt.Errorf("resolve working directory: %w", err)
	}
	if strings.HasSuffix(target, " (deleted)") {
		return "", errors.New("working directory no longer exists")
	}
	return target, nil
}

func (s *Shell) cleanGroup(pgid int) error {
	alive, err := s.groupAlive(pgid)
	if err == nil && !alive {
		return nil
	}
	termErr := signalGroup(pgid, syscall.SIGTERM)
	if err == nil && termErr == nil {
		if stopped, waitErr := s.awaitGroup(pgid, s.termGrace); stopped {
			return nil
		} else if waitErr != nil {
			err = waitErr
		}
	}
	killErr := signalGroup(pgid, syscall.SIGKILL)
	stopped, confirmErr := s.awaitGroup(pgid, s.killGrace)
	if stopped && killErr == nil {
		return nil
	}
	return errors.Join(err, termErr, killErr, confirmErr, errors.New("process group termination not confirmed"))
}

func (s *Shell) awaitGroup(pgid int, timeout time.Duration) (bool, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		alive, err := s.groupAlive(pgid)
		if err != nil || !alive {
			return !alive && err == nil, err
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			return false, nil
		}
	}
}

// Linux may retain orphan zombies under an external reaper. They are already
// terminated and cannot execute or hold output pipes. Include every thread so a
// zombie group leader cannot hide still-running threads.
func processGroupAlive(pgid int) (bool, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		path := "/proc/" + entry.Name()
		data, err := os.ReadFile(path + "/stat")
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			continue
		}
		if err != nil {
			return false, err
		}
		fields := strings.Fields(string(data)[strings.LastIndexByte(string(data), ')')+1:])
		if len(fields) < 3 {
			return false, errors.New("invalid proc stat")
		}
		group, err := strconv.Atoi(fields[2])
		if err != nil {
			return false, err
		}
		if group != pgid {
			continue
		}
		tasks, err := os.ReadDir(path + "/task")
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		for _, task := range tasks {
			data, err := os.ReadFile(path + "/task/" + task.Name() + "/stat")
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
				continue
			}
			if err != nil {
				return false, err
			}
			fields := strings.Fields(string(data)[strings.LastIndexByte(string(data), ')')+1:])
			if len(fields) == 0 {
				return false, errors.New("invalid thread stat")
			}
			if fields[0] != "Z" && fields[0] != "X" {
				return true, nil
			}
		}
	}
	return false, nil
}
