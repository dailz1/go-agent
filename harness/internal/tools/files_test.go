package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/harness/internal/prompt"
	"github.com/dailz1/go-agent/harness/internal/workspace"
	"github.com/dailz1/go-agent/tool"
)

type fixture struct {
	dir   string
	files *Files
	reg   *tool.Registry
}

func setup(t *testing.T, gate WriteGate) fixture {
	t.Helper()
	dir := t.TempDir()
	w, err := workspace.Open(dir, filepath.Join(dir, ".harness"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	rules, err := prompt.Load(w, "", 8192)
	if err != nil {
		t.Fatal(err)
	}
	files := New(w, rules, Options{Writes: gate})
	reg := tool.NewRegistry()
	if err := files.Register(reg); err != nil {
		t.Fatal(err)
	}
	return fixture{dir: dir, files: files, reg: reg}
}

func put(t *testing.T, f fixture, path, data string) {
	t.Helper()
	full := filepath.Join(f.dir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(data), 0640); err != nil {
		t.Fatal(err)
	}
}

func call(t *testing.T, f fixture, name string, args any) *tool.ToolResult {
	t.Helper()
	impl, ok := f.reg.Get(name)
	if !ok {
		t.Fatalf("missing tool %s", name)
	}
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	result, err := impl.Execute(t.Context(), raw)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func decode[T any](t *testing.T, result *tool.ToolResult) T {
	t.Helper()
	if result.IsError() {
		t.Fatal(result.Content)
	}
	var value T
	if err := json.Unmarshal([]byte(result.Content), &value); err != nil {
		t.Fatal(err)
	}
	return value
}

type applyGate struct {
	calls int
	hook  func(workspace.Change)
}

func (g *applyGate) Commit(ctx context.Context, change workspace.Change, apply func() error) error {
	g.calls++
	if g.hook != nil {
		g.hook(change)
	}
	return apply()
}

func TestReadPaginationAndHash(t *testing.T) {
	f := setup(t, nil)
	put(t, f, "a.go", "一\r\ntwo\nthree\n")
	got := decode[ReadObservation](t, call(t, f, "read", map[string]any{"path": "a.go", "offset": 2, "limit": 1}))
	if len(got.Lines) != 1 || got.Lines[0].Number != 2 || got.Lines[0].Text != "two" || !got.Truncated {
		t.Fatalf("page = %+v", got)
	}
	if got.Hash != workspace.Hash([]byte("一\r\ntwo\nthree\n")) {
		t.Fatalf("hash = %q", got.Hash)
	}
}

func TestWriteGateAndPreconditions(t *testing.T) {
	for _, name := range []string{"edit", "write"} {
		t.Run(name, func(t *testing.T) {
			f := setup(t, nil)
			put(t, f, "a", "old")
			read := decode[ReadObservation](t, call(t, f, "read", map[string]any{"path": "a"}))
			args := map[string]any{"path": "a", "expected_hash": read.Hash, "text": "new"}
			if name == "edit" {
				args = map[string]any{"path": "a", "expected_hash": read.Hash, "old_text": "old", "new_text": "new"}
			}
			if !call(t, f, name, args).IsError() {
				t.Fatal("missing gate allowed a write")
			}
			data, err := os.ReadFile(filepath.Join(f.dir, "a"))
			if err != nil || string(data) != "old" {
				t.Fatalf("gate changed disk = %q, %v", data, err)
			}
		})
	}
}

func TestEditRealFileAndRejectStaleHash(t *testing.T) {
	gate := &applyGate{}
	f := setup(t, gate)
	put(t, f, "a", "alpha beta")
	read := decode[ReadObservation](t, call(t, f, "read", map[string]any{"path": "a"}))
	args := map[string]any{"path": "a", "expected_hash": read.Hash, "old_text": "beta", "new_text": "世界"}
	if got := call(t, f, "edit", args); got.IsError() {
		t.Fatal(got.Content)
	}
	data, err := os.ReadFile(filepath.Join(f.dir, "a"))
	if err != nil || string(data) != "alpha 世界" || gate.calls != 1 {
		t.Fatalf("edit = %q, calls %d, %v", data, gate.calls, err)
	}
	if !call(t, f, "edit", args).IsError() || gate.calls != 1 {
		t.Fatal("stale edit reached gate")
	}
}

func TestWriteNewFileAndNestedRulesVersion(t *testing.T) {
	gate := &applyGate{}
	f := setup(t, gate)
	put(t, f, "sub/AGENTS.md", "rules v1")
	args := map[string]any{"path": "sub/new", "text": "hello"}
	if !call(t, f, "write", args).IsError() {
		t.Fatal("write without reading applicable rules accepted")
	}
	decode[ReadObservation](t, call(t, f, "read", map[string]any{"path": "sub/new"}))
	put(t, f, "sub/AGENTS.md", "rules v2")
	if !call(t, f, "write", args).IsError() {
		t.Fatal("stale rules accepted")
	}
	decode[ReadObservation](t, call(t, f, "read", map[string]any{"path": "sub/new"}))
	if got := call(t, f, "write", args); got.IsError() {
		t.Fatal(got.Content)
	}
	if !call(t, f, "write", args).IsError() {
		t.Fatal("overwrite without hash accepted")
	}
}

func TestMalformedArgsAndUnsafeReads(t *testing.T) {
	f := setup(t, nil)
	put(t, f, ".env", "SECRET=value")
	put(t, f, "binary", "a\x00b")
	put(t, f, "large", strings.Repeat("x", workspace.MaxFileBytes+1))
	for _, args := range []string{`{}`, `null`, `{"path":"x","unknown":true}`, `{"path":"x","offset":0}`, `{"path":"x"} {}`} {
		t.Run(args, func(t *testing.T) {
			impl, _ := f.reg.Get("read")
			got, err := impl.Execute(t.Context(), json.RawMessage(args))
			if err != nil || !got.IsError() {
				t.Fatalf("malformed arguments = %v, %v", got, err)
			}
		})
	}
	for _, path := range []string{"../escape", "/etc/passwd", ".env", "binary", "large"} {
		t.Run(path, func(t *testing.T) {
			if !call(t, f, "read", map[string]any{"path": path}).IsError() {
				t.Fatal("unsafe read accepted")
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	impl, _ := f.reg.Get("read")
	if _, err := impl.Execute(ctx, json.RawMessage(`{"path":"x"}`)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}
