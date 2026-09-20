package agenttest

import "github.com/dailz1/go-agent/llm"

func cloneTextDeltaChunk(in *llm.TextDeltaChunk) llm.Chunk {
	if in == nil {
		return (*llm.TextDeltaChunk)(nil)
	}
	out := *in
	return &out
}

func cloneReasoningDeltaChunk(in *llm.ReasoningDeltaChunk) llm.Chunk {
	if in == nil {
		return (*llm.ReasoningDeltaChunk)(nil)
	}
	out := *in
	return &out
}

func cloneToolCallStartChunk(in *llm.ToolCallStartChunk) llm.Chunk {
	if in == nil {
		return (*llm.ToolCallStartChunk)(nil)
	}
	out := *in
	return &out
}

func cloneToolCallArgsChunk(in *llm.ToolCallArgsChunk) llm.Chunk {
	if in == nil {
		return (*llm.ToolCallArgsChunk)(nil)
	}
	out := *in
	return &out
}

func cloneReasoningItemChunk(in *llm.ReasoningItemChunk) llm.Chunk {
	if in == nil {
		return (*llm.ReasoningItemChunk)(nil)
	}
	out := *in
	out.Item.Summary = cloneStrings(in.Item.Summary)
	return &out
}

func cloneDoneChunk(in *llm.DoneChunk) llm.Chunk {
	if in == nil {
		return (*llm.DoneChunk)(nil)
	}
	out := *in
	out.Usage = cloneUsage(in.Usage)
	return &out
}
