package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

const (
	imageOriginal  = "IMGDATA-original"
	imageRewritten = "IMGDATA-rewritten"
)

// bufferTool returns an image result whose Data aliases a caller-owned
// buffer — the shape of a real tool that captures into a reusable buffer.
type bufferTool struct {
	info   tool.ToolInfo
	buffer []byte
}

func (t *bufferTool) Info() tool.ToolInfo { return t.info }

func (t *bufferTool) Execute(_ context.Context, _ json.RawMessage) (*tool.ToolResult, error) {
	return tool.NewImageResult("capture", tool.Image{MIMEType: "image/png", Data: t.buffer}), nil
}

func toolImageFromHistory(history []llm.Message) []byte {
	for _, msg := range history {
		if msg.Role != llm.RoleTool {
			continue
		}
		for _, block := range msg.Content {
			if rb, ok := block.(llm.ToolResultBlock); ok && len(rb.Images) > 0 {
				return rb.Images[0].Data
			}
		}
	}
	return nil
}

func roundRequestToolImage(provider *MockStreamingProvider, round int) []byte {
	for _, msg := range provider.AllMessages[round] {
		if msg.Role != llm.RoleTool {
			continue
		}
		for _, block := range msg.Content {
			if rb, ok := block.(llm.ToolResultBlock); ok && len(rb.Images) > 0 {
				return rb.Images[0].Data
			}
		}
	}
	return nil
}

// runImageCapture streams one tool round + one final text round against a
// no-store agent, handing every ToolResultEvent to during during delivery.
func runImageCapture(t *testing.T, buf []byte, during func(ToolResultEvent)) (*DoneEvent, *MockStreamingProvider) {
	t.Helper()
	capture := &bufferTool{info: tool.ToolInfo{Name: "capture", Description: "captures an image"}, buffer: buf}
	reg := tool.NewRegistry()
	reg.MustRegister(capture)

	provider := NewMockStreamingProvider([][]llm.Chunk{
		{
			llm.ToolCallStartChunk{Index: 0, ID: "c1", Name: "capture"},
			llm.ToolCallArgsChunk{Index: 0, Delta: `{}`},
			llm.DoneChunk{FinishReason: "tool_calls"},
		},
		{
			llm.TextDeltaChunk{Text: "done"},
			llm.DoneChunk{FinishReason: "stop"},
		},
	})

	agent := New(provider, reg, WithLogger(discardLogger()))
	seq, err := agent.RunStream(context.Background(), "run")
	if err != nil {
		t.Fatalf("RunStream returned error: %v", err)
	}

	var done *DoneEvent
	for ev, evErr := range seq {
		if evErr != nil {
			t.Fatalf("stream error: %v", evErr)
		}
		if tr, ok := ev.(ToolResultEvent); ok && during != nil {
			during(tr)
		}
		if d, ok := ev.(DoneEvent); ok {
			done = &d
		}
	}
	if done == nil {
		t.Fatal("stream ended without DoneEvent")
	}
	return done, provider
}

// TestRunStream_ToolResultEventImagesIsolated pins the no-store isolation
// contract (issue #11): a streaming consumer that rewrites the delivered
// ToolResultEvent's image bytes in place cannot change what in-memory
// history retains for the next model round, nor the tool's own buffer.
func TestRunStream_ToolResultEventImagesIsolated(t *testing.T) {
	buf := []byte(imageOriginal)
	var delivered []byte
	done, provider := runImageCapture(t, buf, func(tr ToolResultEvent) {
		delivered = append([]byte(nil), tr.Result.Images[0].Data...)
		tr.Result.Images[0].Data[0] = 'X' // consumer corrupts in place
	})

	if got := string(delivered); got != imageOriginal {
		t.Fatalf("delivered event image = %q, want return-time snapshot %q", got, imageOriginal)
	}
	if got := string(toolImageFromHistory(done.History)); got != imageOriginal {
		t.Errorf("history image = %q, want untouched %q after consumer edit", got, imageOriginal)
	}
	if got := string(roundRequestToolImage(provider, 1)); got != imageOriginal {
		t.Errorf("next-round request image = %q, want untouched %q after consumer edit", got, imageOriginal)
	}
	if got := string(buf); got != imageOriginal {
		t.Errorf("tool buffer = %q, want untouched %q after consumer edit", got, imageOriginal)
	}
}

// TestRunStream_ToolResultBufferReuseIsolated simulates a tool that reuses
// its capture buffer after returning: the buffer is rewritten while the
// result event is being delivered. History, the next round's request, and
// the delivered event must keep the return-time bytes.
func TestRunStream_ToolResultBufferReuseIsolated(t *testing.T) {
	buf := []byte(imageOriginal)
	var delivered []byte
	done, provider := runImageCapture(t, buf, func(tr ToolResultEvent) {
		delivered = append([]byte(nil), tr.Result.Images[0].Data...)
		copy(buf, imageRewritten) // tool reuses its buffer after returning
	})

	if got := string(delivered); got != imageOriginal {
		t.Fatalf("delivered event image = %q, want return-time snapshot %q", got, imageOriginal)
	}
	if got := string(toolImageFromHistory(done.History)); got != imageOriginal {
		t.Errorf("history image = %q, want return-time %q after buffer reuse", got, imageOriginal)
	}
	if got := string(roundRequestToolImage(provider, 1)); got != imageOriginal {
		t.Errorf("next-round request image = %q, want return-time %q after buffer reuse", got, imageOriginal)
	}
}
