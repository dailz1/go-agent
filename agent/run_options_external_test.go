package agent_test

import (
	"io"
	"log/slog"
	"testing"

	"github.com/dailz1/go-agent/agent"
	"github.com/dailz1/go-agent/agenttest"
	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

func TestRunOptionsPublicRequests(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "Run"
		if stream {
			name = "RunStream"
		}
		t.Run(name, func(t *testing.T) {
			base := []llm.Option{llm.WithModel("base"), llm.WithTemperature(0.7)}
			for _, tc := range []struct {
				name   string
				opts   []agent.RunOption
				want   []llm.Option
				prompt string
			}{
				{name: "inherit", want: base, prompt: "A"},
				{name: "merge", opts: []agent.RunOption{
					agent.RunWithLLMOptions(llm.WithTemperature(0.1), llm.WithMaxTokens(42)),
					agent.RunWithSystemPrompt("B"),
				}, want: append(append([]llm.Option{}, base...), llm.WithTemperature(0.1), llm.WithMaxTokens(42)), prompt: "B"},
				{name: "last batch", opts: []agent.RunOption{
					agent.RunWithLLMOptions(llm.WithMaxTokens(99)),
					agent.RunWithLLMOptions(llm.WithTemperature(0.2)),
				}, want: append(append([]llm.Option{}, base...), llm.WithTemperature(0.2)), prompt: "A"},
				{name: "empty batch", opts: []agent.RunOption{
					agent.RunWithLLMOptions(llm.WithMaxTokens(99)), agent.RunWithLLMOptions(),
				}, want: base, prompt: "A"},
				{name: "empty prompt", opts: []agent.RunOption{agent.RunWithSystemPrompt("")}, want: base},
				{name: "last prompt", opts: []agent.RunOption{
					agent.RunWithSystemPrompt("B"), agent.RunWithSystemPrompt("C"),
				}, want: base, prompt: "C"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					exchange := func(prompt string, opts []llm.Option) agenttest.Exchange {
						return agenttest.Exchange{
							Method: agenttest.MethodChatStream,
							Request: agenttest.Request{
								Messages: []llm.Message{llm.SystemMessage(prompt), llm.UserMessage("task")},
								Tools:    []tool.ToolInfo{},
								Options:  llm.ApplyOptions(opts),
							},
							StreamChunks: []llm.Chunk{&llm.TextDeltaChunk{Text: "answer"}, &llm.DoneChunk{FinishReason: "stop"}},
						}
					}
					p := agenttest.NewScriptedProvider(exchange(tc.prompt, tc.want), exchange("A", base))
					a := agent.New(p, tool.NewRegistry(), agent.WithSystemPrompt("A"), agent.WithLLMOptions(base...),
						agent.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
					for _, opts := range [][]agent.RunOption{tc.opts, nil} {
						if stream {
							seq, err := a.RunStream(t.Context(), "task", opts...)
							if err != nil {
								t.Fatal(err)
							}
							done := 0
							for event, err := range seq {
								if err != nil {
									t.Fatal(err)
								}
								if _, ok := event.(agent.DoneEvent); ok {
									done++
								}
							}
							if done != 1 {
								t.Fatalf("Done events=%d", done)
							}
						} else if _, err := a.Run(t.Context(), "task", opts...); err != nil {
							t.Fatal(err)
						}
					}
					if err := p.Verify(); err != nil {
						t.Fatal(err)
					}
				})
			}
		})
	}
}

func TestRunOptionsPromptAbsenceAndSliceCapture(t *testing.T) {
	for _, tc := range []struct {
		name     string
		opts     []agent.RunOption
		messages []llm.Message
	}{
		{name: "omitted constructor prompt", messages: []llm.Message{llm.UserMessage("task")}},
		{name: "explicit empty without constructor prompt", opts: []agent.RunOption{agent.RunWithSystemPrompt("")},
			messages: []llm.Message{llm.SystemMessage(""), llm.UserMessage("task")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caller := []llm.Option{llm.WithModel("captured")}
			captured := agent.RunWithLLMOptions(caller...)
			caller[0] = llm.WithModel("mutated")
			p := agenttest.NewScriptedProvider(agenttest.Exchange{
				Method: agenttest.MethodChatStream,
				Request: agenttest.Request{Messages: tc.messages, Tools: []tool.ToolInfo{},
					Options: llm.ApplyOptions([]llm.Option{llm.WithModel("captured")})},
				StreamChunks: []llm.Chunk{&llm.TextDeltaChunk{Text: "ok"}, &llm.DoneChunk{FinishReason: "stop"}},
			})
			a := agent.New(p, tool.NewRegistry(), agent.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
			if _, err := a.Run(t.Context(), "task", append(tc.opts, captured)...); err != nil {
				t.Fatal(err)
			}
			if err := p.Verify(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
