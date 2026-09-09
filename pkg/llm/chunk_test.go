package llm

import "testing"

// TestChunkSealedInterface verifies at compile time that all five Chunk
// implementations satisfy the sealed interface. If a new type is added
// without implementing chunkType(), this test will fail to compile.
func TestChunkSealedInterface(t *testing.T) {
	t.Parallel()
	var _ Chunk = TextDeltaChunk{}
	var _ Chunk = ReasoningDeltaChunk{}
	var _ Chunk = ToolCallStartChunk{}
	var _ Chunk = ToolCallArgsChunk{}
	var _ Chunk = DoneChunk{}
}
