package modelmeta

import (
	"testing"
)

func TestBundledIncludesCodexAndUsesRecordedTime(t *testing.T) {
	c, err := Bundled()
	if err != nil {
		t.Fatal(err)
	}
	info := c.Info()
	if info.SchemaVersion != 2 || len(info.Sources) != 2 || info.ProviderCount != 325 || info.ModelCount != 9877 {
		t.Fatalf("bundled info = %+v", info)
	}
	windows, err := c.Windows("openai-codex", info.VerifiedAt, 0)
	if err != nil || len(windows) != 2 {
		t.Fatalf("codex = %v, %v", windows, err)
	}
	for _, model := range []string{"gpt-6.1-terra", "gpt-6.1-sol"} {
		if windows[model] != 1050000 {
			t.Fatalf("%s = %d", model, windows[model])
		}
	}
}
