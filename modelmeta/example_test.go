package modelmeta_test

import (
	"fmt"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/llm/codex"
	"github.com/dailz1/go-agent/modelmeta"
)

func ExampleCatalog_Windows() {
	catalog, err := modelmeta.Bundled()
	if err != nil {
		fmt.Println(err)
		return
	}
	// Recorded time makes this offline example independent of today's date.
	windows, err := catalog.Windows("openai-codex", catalog.Info().VerifiedAt, 0)
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(windows["gpt-6.1-terra"])
	// Output: 1050000
}

func ExampleCatalog_Guard() {
	catalog, err := modelmeta.Bundled()
	if err != nil {
		fmt.Println(err)
		return
	}
	windows, err := catalog.Windows("openai-codex", catalog.Info().VerifiedAt, 0)
	if err != nil {
		fmt.Println(err)
		return
	}
	base := codex.NewProvider("gpt-6.1-sol", codex.WithModelContextWindows(windows))
	provider, err := catalog.Guard(base, 0)
	if err != nil {
		fmt.Println(err)
		return
	}
	// Pass provider to agent.New / agent.WithProviders. Guard enforces age at
	// each query, even though the exported map and base provider remain static.
	_, hasWindows := provider.(llm.ContextWindowProvider)
	fmt.Println(hasWindows)
	// Output: true
}
