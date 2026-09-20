package agent

import (
	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

func iterationLimitResults(calls []llm.ToolUseBlock) []llm.Message {
	results := make([]llm.Message, 0, len(calls))
	for _, call := range calls {
		results = append(results, llm.ToolResultMessage(
			call.ID,
			tool.NewErrorResult("not executed: the run reached its iteration limit"),
		))
	}
	return results
}
