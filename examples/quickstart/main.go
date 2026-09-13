// Command quickstart is the checkout-based 10-minute streaming entry point.
//
// From a repository checkout, set OPENAI_API_KEY (and optionally
// OPENAI_BASE_URL and OPENAI_MODEL), then run:
//
//	go run ./examples/quickstart
//
// It calls an OpenAI-compatible API and may incur provider charges. Ask a
// question normally, or explicitly ask it to use the demo echo tool. Tool
// selection is model-dependent; the tool is not a sandbox.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"

	"github.com/dailz1/go-agent/pkg/agent"
	"github.com/dailz1/go-agent/pkg/llm/openai"
	"github.com/dailz1/go-agent/pkg/tool"
)

func main() {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		fmt.Fprintln(os.Stderr, "quickstart: set OPENAI_API_KEY (and optionally OPENAI_BASE_URL, OPENAI_MODEL)")
		os.Exit(1)
	}
	model := os.Getenv("OPENAI_MODEL")
	if model == "" {
		model = "gpt-4o-mini"
	}
	opts := []openai.ProviderOption{}
	if baseURL := os.Getenv("OPENAI_BASE_URL"); baseURL != "" {
		opts = append(opts, openai.WithBaseURL(baseURL))
	}

	registry := tool.NewRegistry()
	registry.MustRegister(echoTool{})
	a := agent.New(openai.NewProvider(key, model, opts...), registry,
		agent.WithSystemPrompt("Be concise. Use echo only when the user explicitly asks you to use the demo tool."),
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	seq, err := a.RunStream(ctx, "Say hello in one sentence. If you can, mention that this response is streaming.")
	if err != nil {
		fatal(err)
	}
	for event, err := range seq {
		if err != nil {
			fatal(err)
		}
		switch event := event.(type) {
		case agent.TextDeltaEvent:
			fmt.Print(event.Text)
		case agent.ToolCallEvent:
			fmt.Fprintf(os.Stderr, "\n[tool] %s\n", event.Name)
		case agent.ToolResultEvent:
			fmt.Fprintf(os.Stderr, "[tool result] error=%v\n", event.Result.IsError())
		case agent.DoneEvent:
			fmt.Println()
			fmt.Fprintf(os.Stderr, "[done] tool calls=%d truncated=%v\n", event.ToolCalls, event.Truncated)
		}
	}
}

type echoTool struct{}

func (echoTool) Info() tool.ToolInfo {
	schema := tool.NewParameterSchema()
	schema.Properties["text"] = tool.Param("string", "text to return unchanged")
	schema.Required = []string{"text"}
	return tool.ToolInfo{Name: "echo", Description: "Returns supplied text unchanged; use only when explicitly requested.", Parameters: schema}
}

func (echoTool) Execute(_ context.Context, args json.RawMessage) (*tool.ToolResult, error) {
	var input struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return tool.NewErrorResult("invalid arguments: %v", err), nil
	}
	return tool.NewTextResult(input.Text), nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "quickstart:", err)
	os.Exit(1)
}
