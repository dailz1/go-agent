package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

func newImageResultAgent(t *testing.T, st store.Store) *Agent {
	t.Helper()
	registry := tool.NewRegistry()
	registry.MustRegister(&mockTool{
		info:   tool.ToolInfo{Name: "screenshot"},
		result: tool.NewImageResult("screenshot taken", tool.Image{URL: "https://example.com/a.png"}, tool.Image{MIMEType: "image/png", Data: []byte("png-bytes")}),
	})
	provider := NewMockProvider(
		MsgResponse(llm.AssistantToolCallMessage(llm.ToolUseBlock{Type: "tool_use", ID: "call-1", Name: "screenshot", Input: []byte(`{}`)})),
		MsgResponse(llm.AssistantMessage("done")),
	)
	options := []Option{WithLogger(discardLogger())}
	if st != nil {
		options = append(options, WithStore(st))
	}
	return New(provider, registry, options...)
}

// TestAgentToolResultImagesEndToEnd pins that a tool returning an image
// result carries the images through the event stream and into the
// authoritative DoneEvent.History (T6).
func TestAgentToolResultImagesEndToEnd(t *testing.T) {
	agent := newImageResultAgent(t, nil)
	result, err := agent.Run(context.Background(), "take a screenshot")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var images []llm.ImageBlock
	for _, message := range result.History {
		for _, block := range message.Content {
			if tr, ok := block.(llm.ToolResultBlock); ok {
				images = tr.Images
			}
		}
	}
	if len(images) != 2 || images[0].URL != "https://example.com/a.png" ||
		images[1].MIMEType != "image/png" || string(images[1].Data) != "png-bytes" {
		t.Fatalf("history images = %+v", images)
	}
}

// TestDeepCopyClonesToolResultImages pins that deepCopyMessages clones
// ToolResultBlock images in both value and pointer forms so caller mutations
// cannot leak into the copied history (T7).
func TestDeepCopyClonesToolResultImages(t *testing.T) {
	src := []llm.Message{{Role: llm.RoleTool, Content: []llm.ContentBlock{
		llm.ToolResultBlock{Type: "tool_result", ToolUseID: "call_1", Content: "v",
			Images: []llm.ImageBlock{{Type: "image", URL: "https://example.com/v.png"}}},
		&llm.ToolResultBlock{Type: "tool_result", ToolUseID: "call_2", Content: "p",
			Images: []llm.ImageBlock{{Type: "image", MIMEType: "image/png", Data: []byte("png")}}},
	}}}
	dst := deepCopyMessages(src)
	// Mutate the originals after the copy.
	src[0].Content[0].(llm.ToolResultBlock).Images[0].URL = "mutated"
	src[0].Content[1].(*llm.ToolResultBlock).Images[0].Data[0] = 'X'

	vb := dst[0].Content[0].(llm.ToolResultBlock)
	if vb.Images[0].URL != "https://example.com/v.png" {
		t.Errorf("value-form Images leaked: %q", vb.Images[0].URL)
	}
	pb := dst[0].Content[1].(*llm.ToolResultBlock)
	if string(pb.Images[0].Data) != "png" {
		t.Errorf("pointer-form Data leaked: %q", pb.Images[0].Data)
	}
}

// TestImageResultPersistsAndReplays pins the round_committed round trip:
// images survive encode into the record log and decode on replay (T8).
func TestImageResultPersistsAndReplays(t *testing.T) {
	st := store.NewMemory()
	agent := newImageResultAgent(t, st)
	if _, err := agent.Run(context.Background(), "take a screenshot", WithThread("t")); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var decoded []llm.Message
	for _, record := range logRecords(t, st, "t") {
		if record.Kind != store.KindRoundCommitted {
			continue
		}
		payload, err := decodeRoundCommitted(record)
		if err != nil {
			t.Fatalf("decodeRoundCommitted: %v", err)
		}
		decoded = append(decoded, payload.Results...)
	}
	var images []llm.ImageBlock
	for _, message := range decoded {
		for _, block := range message.Content {
			if tr, ok := block.(llm.ToolResultBlock); ok {
				images = tr.Images
			}
		}
	}
	if len(images) != 2 || images[0].URL != "https://example.com/a.png" || string(images[1].Data) != "png-bytes" {
		t.Fatalf("replayed images = %+v", images)
	}
}

// TestTruncateToolResultKeepsImages pins that the truncation channel only
// reshapes the text content and never touches image attachments (T9).
func TestTruncateToolResultKeepsImages(t *testing.T) {
	result := tool.NewImageResult(strings.Repeat("x", 500), tool.Image{URL: "https://example.com/a.png"})
	truncated, omitted := truncateToolResult(result, 200)
	if truncated == result {
		t.Fatal("truncation must not mutate the input result")
	}
	if omitted == 0 {
		t.Fatal("omitted = 0, want the head+tail omission marker active")
	}
	if len(truncated.Content) > 50+200 { // head+tail with omission marker
		t.Fatalf("content not truncated: %d runes", len([]rune(truncated.Content)))
	}
	if len(truncated.Images) != 1 || truncated.Images[0].URL != "https://example.com/a.png" {
		t.Fatalf("images = %+v, want preserved verbatim", truncated.Images)
	}
}
