package app

import (
	"testing"

	"github.com/dailz1/go-agent/harness/internal/config"
)

func prepareStartup(t *testing.T, noApproval bool) *Startup {
	t.Helper()
	cfg, err := config.Parse([]string{"--workspace", t.TempDir(), "--model", "test"}, func(string) string { return "" })
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
