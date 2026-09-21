package app

import (
	"encoding/json"
	"testing"

	"github.com/dailz1/go-agent/harness/internal/config"
)

func TestStartupRegistersOnlyGatedFileTools(t *testing.T) {
	cfg, err := config.Parse([]string{"--workspace", t.TempDir(), "--model", "test", "--no-approval"}, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := Prepare(cfg, func(string) string { return "test-key" })
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if runtime.Provider.Name() != "openai" || len(runtime.Registry.List()) != 5 {
		t.Fatal("provider or file tools not wired")
	}
	if _, ok := runtime.Registry.Get("shell"); ok {
		t.Fatal("Stage B must not register shell")
	}
	read, _ := runtime.Registry.Get("read")
	if _, err := read.Execute(t.Context(), json.RawMessage(`{"path":"new"}`)); err != nil {
		t.Fatal(err)
	}
	write, _ := runtime.Registry.Get("write")
	result, err := write.Execute(t.Context(), json.RawMessage(`{"path":"new","text":"forbidden"}`))
	if err != nil || !result.IsError() {
		t.Fatalf("no-approval bypassed missing snapshot gate: %v, %v", result, err)
	}
}
