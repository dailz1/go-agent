package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dailz1/go-agent/modelmeta/internal/source"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

const ordinary = `{"z":{"models":{"b":{"limit":{"context":20}},"a":{"limit":{"context":10}}}}}`
const codexBody = `{"openai-codex":{"models":{"m":{"limit":{"context":30}}}}}`

func commandTime() time.Time { return time.Date(2026, 11, 20, 1, 2, 3, 0, time.UTC) }

func commandClient(failAt int) *http.Client {
	calls := 0
	return &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		body, status := ordinary, 200
		if calls == 1 {
			body = codexBody
		}
		if calls == failAt {
			status = 500
		}
		calls++
		return &http.Response{
			StatusCode: status, Body: io.NopCloser(strings.NewReader(body)),
			Header: http.Header{"Etag": {fmt.Sprint(calls)}},
		}, nil
	})}
}

func TestRunCanonicalSnapshotIsRepeatable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "windows.json")
	want := fmt.Sprintf(
		`{"schema_version":2,"sources":[{"etag":"1","id":"models-dev-v1","sha256":"%x","skipped_models":0,"url":"%s","verified_at":"2026-11-20T01:02:03Z"},{"etag":"2","id":"models-dev-v2-codex","sha256":"%x","skipped_models":0,"url":"%s","verified_at":"2026-11-20T01:02:03Z"}],"verified_at":"2026-11-20T01:02:03Z","windows":{"openai-codex":{"m":30},"z":{"a":10,"b":20}}}`+"\n",
		sha256.Sum256([]byte(ordinary)),
		source.V1URL,
		sha256.Sum256([]byte(codexBody)),
		source.V2URL,
	)
	for range 2 {
		if err := run(t.Context(), []string{"-output", path}, commandClient(-1), commandTime); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, []byte(want)) {
			t.Fatalf("snapshot = %s, %v; want %s", got, err, want)
		}
	}
}

func TestRunFailurePreservesTargetAndCleansTemporaryFiles(t *testing.T) {
	for _, failAt := range []int{0, 1} {
		t.Run(fmt.Sprintf("source%d", failAt), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "windows.json")
			if err := os.WriteFile(path, []byte("old bytes"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := run(t.Context(), []string{"-output", path}, commandClient(failAt), commandTime); err == nil {
				t.Fatal("failed update succeeded")
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != "old bytes" {
				t.Fatalf("target changed: %s, %v", got, err)
			}
			files, err := os.ReadDir(dir)
			if err != nil || len(files) != 1 {
				t.Fatalf("temporary files remain: %v, %v", files, err)
			}
		})
	}
	dir := t.TempDir()
	for _, target := range []string{dir, filepath.Join(dir, "missing", "windows.json")} {
		if err := replace(target, []byte("new")); err == nil {
			t.Fatalf("replacement %q should fail", target)
		}
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 0 {
		t.Fatalf("rename failure left temporary file: %v, %v", files, err)
	}
}

func TestRunRequiresExplicitTargetBeforeFetching(t *testing.T) {
	client := &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid invocation performed network I/O")
		return nil, nil
	})}
	for _, args := range [][]string{nil, {"-unknown"}, {"-output", "x", "extra"}} {
		if err := run(context.Background(), args, client, commandTime); err == nil {
			t.Fatalf("arguments %v accepted", args)
		}
	}
}
