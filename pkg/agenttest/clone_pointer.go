package agenttest

import "github.com/dailz1/go-agent/pkg/llm"

func cloneTextBlock(in *llm.TextBlock) llm.ContentBlock {
	if in == nil {
		return (*llm.TextBlock)(nil)
	}
	out := *in
	return &out
}

func cloneReasoningBlock(in *llm.ReasoningBlock) llm.ContentBlock {
	if in == nil {
		return (*llm.ReasoningBlock)(nil)
	}
	out := *in
	return &out
}

func cloneImageBlock(in *llm.ImageBlock) llm.ContentBlock {
	if in == nil {
		return (*llm.ImageBlock)(nil)
	}
	out := *in
	out.Data = cloneBytes(in.Data)
	return &out
}

func cloneToolUseBlock(in *llm.ToolUseBlock) llm.ContentBlock {
	if in == nil {
		return (*llm.ToolUseBlock)(nil)
	}
	out := *in
	out.Input = cloneRaw(in.Input)
	return &out
}

func cloneReasoningItemBlock(in *llm.ReasoningItemBlock) llm.ContentBlock {
	if in == nil {
		return (*llm.ReasoningItemBlock)(nil)
	}
	out := *in
	out.Summary = cloneStrings(in.Summary)
	return &out
}

func cloneToolResultBlock(in *llm.ToolResultBlock) llm.ContentBlock {
	if in == nil {
		return (*llm.ToolResultBlock)(nil)
	}
	out := *in
	return &out
}
