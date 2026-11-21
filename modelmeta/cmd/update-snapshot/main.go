// Command update-snapshot explicitly refreshes the bundled models.dev snapshot.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/dailz1/go-agent/modelmeta/internal/source"
)

func main() {
	if err := run(context.Background(), os.Args[1:], nil, time.Now); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, client *http.Client, now func() time.Time) error {
	flags := flag.NewFlagSet("update-snapshot", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	target := flags.String("output", "", "snapshot file to replace (required)")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("parse arguments: %w", err)
	}
	if *target == "" || flags.NArg() != 0 {
		return fmt.Errorf("usage: update-snapshot -output path")
	}
	s, err := source.Fetch(ctx, client, now)
	if err != nil {
		return fmt.Errorf("refresh snapshot: %w", err)
	}
	data, err := source.Canonical(s)
	if err != nil {
		return fmt.Errorf("serialize snapshot: %w", err)
	}
	return replace(*target, data)
}

func replace(path string, data []byte) (err error) {
	f, err := os.CreateTemp(filepath.Dir(path), ".windows-*.json")
	if err != nil {
		return fmt.Errorf("create temporary snapshot: %w", err)
	}
	defer func() {
		if removeErr := os.Remove(f.Name()); removeErr != nil && !os.IsNotExist(removeErr) && err == nil {
			err = fmt.Errorf("remove temporary snapshot: %w", removeErr)
		}
	}()
	if err := f.Chmod(0o644); err != nil {
		return fmt.Errorf("set snapshot mode: %w", errorsWithClose(f, err))
	}
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write snapshot: %w", errorsWithClose(f, err))
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync snapshot: %w", errorsWithClose(f, err))
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close snapshot: %w", err)
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return fmt.Errorf("replace snapshot: %w", err)
	}
	return nil
}

func errorsWithClose(f *os.File, err error) error {
	if closeErr := f.Close(); closeErr != nil {
		return fmt.Errorf("%w; close snapshot: %w", err, closeErr)
	}
	return err
}
