package agent

import (
	"slices"

	"github.com/dailz1/go-agent/llm"
)

// Resolve user functions once per round. Each consumer gets its own copy so
// a provider or compactor cannot mutate the options of a later invocation.
func freezeLLMOptions(opts []llm.Option) []llm.Option {
	snapshot := cloneLLMOptions(llm.ApplyOptions(opts))
	return []llm.Option{func(o *llm.Options) { *o = cloneLLMOptions(snapshot) }}
}

func cloneLLMOptions(o llm.Options) llm.Options {
	o.Stop = slices.Clone(o.Stop)
	o.Temperature = cloneOptionValue(o.Temperature)
	o.MaxCompletionTokens = cloneOptionValue(o.MaxCompletionTokens)
	o.TopP = cloneOptionValue(o.TopP)
	o.Seed = cloneOptionValue(o.Seed)
	o.PresencePenalty = cloneOptionValue(o.PresencePenalty)
	o.FrequencyPenalty = cloneOptionValue(o.FrequencyPenalty)
	o.ParallelToolCalls = cloneOptionValue(o.ParallelToolCalls)
	o.ToolChoice = cloneOptionValue(o.ToolChoice)
	return o
}

func cloneOptionValue[T any](p *T) *T {
	if p == nil {
		return nil
	}
	value := *p
	return &value
}
