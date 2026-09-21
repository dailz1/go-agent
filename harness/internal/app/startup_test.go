package app

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/harness/internal/config"
)

func prepareStartup(t *testing.T, noApproval bool) *Startup {
	t.Helper()
	// --data-dir keeps the log file hermetic: the fake getenv below answers
	// "test-key" for every variable, including XDG_DATA_HOME, which would
	// otherwise resolve the data dir to a CWD-relative "test-key/..." path.
	cfg, err := config.Parse([]string{"--workspace", t.TempDir(), "--model", "test",
		"--data-dir", t.TempDir()}, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	cfg.NoApproval = noApproval
	runtime, err := Prepare(cfg, func(string) string { return "test-key" })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runtime.Close() })
	return runtime
}

func TestStartupOwnsApprovalMode(t *testing.T) {
	for _, tt := range []struct {
		name       string
		noApproval bool
	}{
		{name: "per-call default", noApproval: false},
		{name: "explicit no-approval", noApproval: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			runtime := prepareStartup(t, tt.noApproval)
			if runtime.Approvals == nil || runtime.Approvals.noApproval != tt.noApproval {
				t.Fatal("approval mode not wired from startup configuration")
			}
		})
	}
}

func TestBeginRunFailsClosedWithoutStageCStores(t *testing.T) {
	runtime := prepareStartup(t, true)
	if _, _, err := runtime.BeginRun(t.Context(), "thread", nil, nil); err == nil {
		t.Fatal("run began without snapshot and output stores")
	}
}

func TestBeginRunRegistersGatedTools(t *testing.T) {
	runtime := prepareStartup(t, true)
	stores := openStores(t, runtime)
	run, registry, err := runtime.BeginRun(t.Context(), "thread", stores.snapshots, stores.outputs)
	if err != nil {
		t.Fatal(err)
	}
	defer run.Cancel()
	for _, name := range []string{"read", "glob", "grep", "edit", "write", "shell"} {
		if _, ok := registry.Get(name); !ok {
			t.Fatalf("tool %s not registered", name)
		}
	}
}

// The TUI owns the terminal exclusively; kernel and provider logs must land in
// the data-dir log file, never on stderr (which is the TUI's terminal).
func TestStartupRoutesDefaultLogsToFile(t *testing.T) {
	prev := slog.Default()
	defer slog.SetDefault(prev)

	dataDir := t.TempDir()
	ws := t.TempDir()
	cfg, err := config.Parse([]string{"--workspace", ws, "--model", "m", "--data-dir", dataDir},
		func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := Prepare(cfg, func(name string) string {
		if name == cfg.APIKeyEnv {
			return "test-key"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	slog.Default().Info("stagef-log-probe", "kind", "acceptance")
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(dataDir, "logs", "harness.log"))
	if err != nil {
		t.Fatalf("harness log missing: %v", err)
	}
	if !strings.Contains(string(data), "stagef-log-probe") {
		t.Fatalf("probe line not in harness log; got: %q", data)
	}
	info, err := os.Stat(filepath.Join(dataDir, "logs", "harness.log"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("harness log perm = %o, want 600", perm)
	}
}
